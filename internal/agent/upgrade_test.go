/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent //nolint:testpackage // White-box tests exercise the agent internals.

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"

	rpcapi "koujouo.anqou.net/project/rpc"
)

// Fixtures of the upgrade tests.
const (
	// upgradeBinary stands in for the new agent binary.
	upgradeBinary = "#!/kojouo/agent new version\n"

	// chunkSize is the test write size of the upgrade stream.
	chunkSize = 4

	// echoGameScript is the "game" of the exec handoff test: it prints
	// a join line for every line it reads and exits cleanly on SIGINT,
	// like the real game after saving.
	echoGameScript = "trap 'exit 0' INT TERM\nwhile read line; do echo \"   1.000 [JOIN] $line joined the game\"; done\n"

	// waitingChild is a child that stays alive without forking.
	waitingChild = "exec sleep 30"

	// exitingChild is a child that exits at once.
	exitingChild = "exit 0"

	// adoptedPlayer is the player the handoff test feeds the game.
	adoptedPlayer = "tester"

	// handoffAdoptDeadline bounds the wait for a child state.
	handoffAdoptDeadline = 5 * time.Second
)

// errExecStubbed is returned by the recording exec hook, so the Done
// call of an upgrade reports a failure instead of replacing the test
// process.
var errExecStubbed = errors.New("exec stubbed")

// execRecord captures an upgrade exec without replacing the process.
type execRecord struct {
	mutex sync.Mutex
	path  string
	argv  []string
	env   []string
}

// record implements the ExecBinary hook of the agent options.
func (r *execRecord) record(path string, argv []string, env []string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.path, r.argv, r.env = path, argv, env

	return errExecStubbed
}

// recorded returns the captured exec call.
func (r *execRecord) recorded() (string, []string, []string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.path, r.argv, r.env
}

// TestAgentUpgradeRoundTrip streams a small binary over the ByteStream
// capability and verifies the installed file, the exec environment and
// that the game keeps running.
func TestAgentUpgradeRoundTrip(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	recorder := &execRecord{} //nolint:exhaustruct_v5 // Starts with no recorded exec.

	client, gameAgent := newRPCAgent(t, logr.Discard(), roundTripOptions(t.TempDir(), cacheDir, recorder.record))

	t.Cleanup(func() { killGame(gameAgent) })

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	game := runningGame(gameAgent)

	pid, stdoutFd := game.Pid(), game.StdoutFd()

	binary := []byte(upgradeBinary)

	sum := sha256.Sum256(binary)

	err := pushUpgrade(t, client, binary, sum[:])
	if err == nil || !strings.Contains(err.Error(), errExecStubbed.Error()) {
		t.Fatalf("upgrade error = %v, want the stubbed exec error", err)
	}

	probeErr := probeProcess(pid)
	if probeErr != nil {
		t.Fatalf("the game process did not survive the upgrade: %v", probeErr)
	}

	installed := readFile(t, filepath.Join(cacheDir, agentNextName))
	if installed != upgradeBinary {
		t.Errorf("installed agent binary = %q, want %q", installed, upgradeBinary)
	}

	info, statErr := os.Stat(filepath.Join(cacheDir, agentNextName))
	if statErr != nil {
		t.Fatalf("could not stat the installed agent binary: %v", statErr)
	}

	if info.Mode().Perm() != binaryFileMode {
		t.Errorf("installed agent binary mode = %v, want %o", info.Mode().Perm(), binaryFileMode)
	}

	assertExecHandoff(t, gameAgent, recorder, pid, stdoutFd)
}

// assertExecHandoff verifies the environment of the exec and that the
// game output survives it.
func assertExecHandoff(
	t *testing.T,
	gameAgent *Agent,
	recorder *execRecord,
	pid int,
	stdoutFd uintptr,
) {
	t.Helper()

	execPath, execArgv, execEnv := recorder.recorded()

	if want := filepath.Join(gameAgent.cacheDir, agentNextName); execPath != want {
		t.Errorf("exec path = %q, want %q", execPath, want)
	}

	if !slices.Equal(execArgv, os.Args) {
		t.Errorf("exec argv = %q, want the arguments of this agent %q", execArgv, os.Args)
	}

	if !slices.Contains(execEnv, envFactorioPid+"="+strconv.Itoa(pid)) ||
		!slices.Contains(execEnv, envFactorioStdoutFd+"="+strconv.Itoa(int(stdoutFd))) {
		t.Errorf("exec env = %v, want the game pid and stdout descriptor", execEnv)
	}

	game := runningGame(gameAgent)

	assertOutputSurvivesExec(t, game)
}

// assertOutputSurvivesExec verifies that the game output descriptors
// are no longer closed on exec, so they survive the exec(2) of a real
// upgrade.
func assertOutputSurvivesExec(t *testing.T, game gameHandle) {
	t.Helper()

	stdout, stdoutIsFile := game.Stdout().(*os.File)
	stderr, stderrIsFile := game.Stderr().(*os.File)

	if !stdoutIsFile || !stderrIsFile {
		t.Fatalf("the game output is not file backed: %T, %T", game.Stdout(), game.Stderr())
	}

	if hasCloexec(stdout) || hasCloexec(stderr) {
		t.Error("the game output descriptors are still closed on exec")
	}
}

// TestAgentUpgradeRejectsABadChecksum verifies that a binary whose
// sha256 does not match is neither installed nor exec'd.
func TestAgentUpgradeRejectsABadChecksum(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	recorder := &execRecord{} //nolint:exhaustruct_v5 // Starts with no recorded exec.

	client, gameAgent := newRPCAgent(t, logr.Discard(), roundTripOptions(t.TempDir(), cacheDir, recorder.record))

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	t.Cleanup(func() { killGame(gameAgent) })

	sum := sha256.Sum256([]byte("something else"))

	err := pushUpgrade(t, client, []byte(upgradeBinary), sum[:])
	if err == nil || !strings.Contains(err.Error(), errUpgradeVerify.Error()) {
		t.Fatalf("upgrade error = %v, want the verification error", err)
	}

	if execPath, _, _ := recorder.recorded(); execPath != "" {
		t.Errorf("the new agent binary was exec'd from %q despite the bad checksum", execPath)
	}

	entries, readErr := os.ReadDir(cacheDir)
	if readErr != nil {
		t.Fatalf("could not read the cache directory: %v", readErr)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), agentNextName) {
			t.Errorf("a failed upgrade left %q behind", entry.Name())
		}
	}

	if pid := gamePid(gameAgent); pid <= 0 {
		t.Error("the rejected upgrade stopped the game")
	}
}

// TestAgentResumeAfterAnExecHandoff simulates what the new agent
// process finds after an upgrade exec(2): the pid and the output
// descriptors of the running game in the environment, and the cached
// state on the save volume. It must re-attach to the game.
func TestAgentResumeAfterAnExecHandoff(t *testing.T) { //nolint:paralleltest // t.Setenv requires a sequential test.
	webhook, requests := newCapturingWebhook(t, httpStatusNoContent)

	saveDir := t.TempDir()

	game := stageExecHandoff(t, saveDir, webhook.URL)

	log, sink := newLogSink()

	gameAgent, err := New(log, roundTripOptions(saveDir, t.TempDir(), nil))
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	gameAgent.Resume(t.Context())

	adopted := runningGame(gameAgent)
	if adopted == nil {
		t.Fatalf("the agent did not adopt the game (phase %s)", gameAgent.snapshot().phase)
	}

	t.Cleanup(func() { killGame(gameAgent) })

	if adopted.Pid() != game.cmd.Process.Pid {
		t.Errorf("adopted game pid = %d, want %d", adopted.Pid(), game.cmd.Process.Pid)
	}

	status := gameAgent.snapshot()
	if status.phase != rpcapi.Phase_running || status.gameVersion != testGameVersion {
		t.Errorf("status after the handoff = %s %q, want running %q",
			status.phase, status.gameVersion, testGameVersion)
	}

	// The output of the adopted game keeps flowing into the agent.
	_, err = io.WriteString(game.stdin, adoptedPlayer+"\n")
	if err != nil {
		t.Fatalf("could not feed the adopted game: %v", err)
	}

	sink.waitForLog(t, "stream=stdout line=1.000 [JOIN] "+adoptedPlayer+" joined the game")
	assertNotification(t, requests, discordMessage(adoptedPlayer+" joined the game"))

	// A graceful stop reaches the adopted game, which exits cleanly.
	err = gameAgent.stop(t.Context())
	if err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	assertSuspended(t, gameAgent)
}

// stageExecHandoff prepares what the previous agent process leaves
// behind right before the exec(2) of an upgrade: the cached state on
// the save volume, and the pid and the output descriptors of its still
// running game in the environment. It returns the running game.
func stageExecHandoff(t *testing.T, saveDir, webhookURL string) *echoGame {
	t.Helper()

	config := roundTripConfig()
	config.DiscordWebhookURL = webhookURL

	err := saveState(saveDir, &persistedState{
		Config:       config,
		RconPort:     rconDefaultPort,
		RconPassword: testRconPassword,
	})
	if err != nil {
		t.Fatalf("could not write the state of the previous agent: %v", err)
	}

	game := startEchoGame(t)

	t.Setenv(envFactorioPid, strconv.Itoa(game.cmd.Process.Pid))
	t.Setenv(envFactorioStdoutFd, strconv.Itoa(int(game.stdout.Fd())))
	t.Setenv(envFactorioStderrFd, strconv.Itoa(int(game.stderr.Fd())))

	return game
}

// echoGame is a fake game started the way the previous agent process
// started it: with its output on pipes.
type echoGame struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File
	stderr *os.File
}

// handoffOutputs holds the read ends of the output pipes the handoff
// test hands to the agent. The agent wraps their descriptors in os.File
// values of its own, as the exec'd process does after a real upgrade,
// so this process must never close them a second time: they stay
// pinned here until the test binary exits.
var handoffOutputs []*os.File //nolint:gochecknoglobals // Pins descriptors that must not be closed twice.

// startEchoGame starts the echo game the way the agent starts the real
// game: its output goes to pipes, and the parent closes its write ends
// once the game runs. The test owns the stdin pipe.
func startEchoGame(t *testing.T) *echoGame {
	t.Helper()

	cmd := exec.Command("sh", "-c", echoGameScript) //nolint:noctx // A short local shell run.

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("could not create the stdin pipe: %v", err)
	}

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("could not create the stdout pipe: %v", err)
	}

	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("could not create the stderr pipe: %v", err)
	}

	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	err = cmd.Start()
	if err != nil {
		t.Fatalf("could not start the echo game: %v", err)
	}

	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	handoffOutputs = append(handoffOutputs, stdout, stderr)

	game := &echoGame{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}

	// An agent that adopted the game reclaims it and its Wait fails
	// harmlessly; this reclaims a game nobody waited for.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return game
}

// TestParsePidEnv verifies which environments describe a game to adopt.
func TestParsePidEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		wantPID  int
		wantOut  uintptr
		wantErrs uintptr
		wantOK   bool
	}{
		{name: "no pid", env: map[string]string{}, wantPID: 0, wantOut: 0, wantErrs: 0, wantOK: false},
		{
			name:    "complete",
			env:     map[string]string{envFactorioPid: "39", envFactorioStdoutFd: "3", envFactorioStderrFd: "5"},
			wantPID: 39, wantOut: 3, wantErrs: 5, wantOK: true,
		},
		{
			name:    "pid only",
			env:     map[string]string{envFactorioPid: "39"},
			wantPID: 0, wantOut: 0, wantErrs: 0, wantOK: false,
		},
		{
			name:    "a standard stream is no game output",
			env:     map[string]string{envFactorioPid: "39", envFactorioStdoutFd: "1", envFactorioStderrFd: "5"},
			wantPID: 0, wantOut: 0, wantErrs: 0, wantOK: false,
		},
		{
			name:    "negative pid",
			env:     map[string]string{envFactorioPid: "-1", envFactorioStdoutFd: "3", envFactorioStderrFd: "4"},
			wantPID: 0, wantOut: 0, wantErrs: 0, wantOK: false,
		},
		{
			name:    "garbage descriptor",
			env:     map[string]string{envFactorioPid: "39", envFactorioStdoutFd: "x", envFactorioStderrFd: "4"},
			wantPID: 0, wantOut: 0, wantErrs: 0, wantOK: false,
		},
	}

	for _, test := range tests {
		pid, stdoutFd, stderrFd, ok := parsePidEnv(func(key string) string { return test.env[key] })

		if pid != test.wantPID || stdoutFd != test.wantOut || stderrFd != test.wantErrs || ok != test.wantOK {
			t.Errorf("%s: parsePidEnv() = (%d, %d, %d, %v), want (%d, %d, %d, %v)",
				test.name, pid, stdoutFd, stderrFd, ok,
				test.wantPID, test.wantOut, test.wantErrs, test.wantOK)
		}
	}
}

// TestReapIfExited verifies the three answers about a pid: a live child
// is adopted, a child that already exited is gone, and a process that
// is not a child is gone too.
func TestReapIfExited(t *testing.T) {
	t.Parallel()

	// PID 1 is never a child of the test process.
	initErr := reapIfExited(1)
	if initErr == nil {
		t.Error("reapIfExited(init) error = nil, want an error")
	}

	alive := startChild(t, waitingChild)

	probeErr := reapIfExited(alive.Process.Pid)
	if probeErr != nil {
		t.Errorf("reapIfExited(live child) error = %v, want nil", probeErr)
	}

	// The probe must not have reaped the live child.
	againErr := reapIfExited(alive.Process.Pid)
	if againErr != nil {
		t.Errorf("reapIfExited(live child, again) error = %v, want nil", againErr)
	}

	exited := startChild(t, exitingChild)

	deadline := time.Now().Add(handoffAdoptDeadline)

	for {
		err := reapIfExited(exited.Process.Pid)
		if err != nil {
			if !errors.Is(err, errGameGone) {
				t.Errorf("reapIfExited(exited child) error = %v, want %v", err, errGameGone)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the child never exited")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// startChild starts a shell running script and makes sure it does not
// outlive the test. The caller may probe it with reapIfExited.
func startChild(t *testing.T, script string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command("sh", "-c", script) //nolint:noctx,gosec // Local shell run.

	err := cmd.Start()
	if err != nil {
		t.Fatalf("could not start the child: %v", err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return cmd
}

// TestClearCloexec verifies that an upgrade makes a descriptor survive
// the exec(2) without touching its other flags.
func TestClearCloexec(t *testing.T) {
	t.Parallel()

	reader, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("could not create the pipe: %v", err)
	}

	defer reader.Close() //nolint:errcheck // Read-only, nothing to flush.

	if !hasCloexec(reader) {
		t.Fatal("a fresh pipe descriptor is not closed on exec, the test cannot tell the difference")
	}

	err = clearCloexec(reader)
	if err != nil {
		t.Fatalf("clearCloexec() error = %v", err)
	}

	if hasCloexec(reader) {
		t.Error("the descriptor is still closed on exec")
	}
}

// hasCloexec reports whether the descriptor of the file is closed on
// exec.
func hasCloexec(file *os.File) bool {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_GETFD, 0)
	if errno != 0 {
		return false
	}

	return flags&syscall.FD_CLOEXEC != 0
}

// pushUpgrade streams the binary through the ByteStream capability of
// an upgrade call.
func pushUpgrade(t *testing.T, client rpcapi.Agent, binary []byte, sum []byte) error {
	t.Helper()

	future, release := client.Upgrade(t.Context(), nil)

	results, err := future.Struct()
	if err != nil {
		release()

		t.Fatalf("upgrade call failed: %v", err)
	}

	err = writeStream(t.Context(), results.Bin(), binary, sum)

	release()

	return err
}

// writeStream pushes the chunks of the new binary through the ByteStream
// capability and ends the stream with done(sha256).
func writeStream(ctx context.Context, stream rpcapi.ByteStream, binary []byte, sum []byte) error {
	for offset := 0; offset < len(binary); offset += chunkSize {
		start, end := offset, min(offset+chunkSize, len(binary))

		err := stream.Write(ctx, func(params rpcapi.ByteStream_write_Params) error {
			return params.SetChunk(binary[start:end])
		})
		if err != nil {
			return err //nolint:wrapcheck // The test reports the raw stream error.
		}
	}

	done, release := stream.Done(ctx, func(params rpcapi.ByteStream_done_Params) error {
		return params.SetSha256(sum)
	})
	defer release()

	_, err := done.Struct()

	return err //nolint:wrapcheck // The test reports the raw stream error.
}

// probeProcess verifies that the process is still running.
func probeProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err //nolint:wrapcheck // The test wants the raw probe result.
	}

	return process.Signal(syscall.Signal(0)) //nolint:wrapcheck // The test wants the raw probe result.
}
