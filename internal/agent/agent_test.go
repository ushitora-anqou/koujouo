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
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	capnprpc "capnproto.org/go/capnp/v3/rpc"
	"github.com/go-logr/logr"

	"koujouo.anqou.net/project/internal/version"
	rpcapi "koujouo.anqou.net/project/rpc"
)

// Fixtures of the agent round trips over a real Cap'n Proto connection.
const (
	// testNewGameVersion is the version a round trip upgrades to.
	testNewGameVersion = "2.1.25"

	// readyMarkerSuffix starts the marker the fake game writes next to
	// the save once it serves. It ends with the process id, so the
	// marker of a previous run never satisfies a restart.
	readyMarkerSuffix = ".ready."

	// roundTripDrainPoll and roundTripSaveWait keep the round trips
	// fast.
	roundTripDrainPoll = 10 * time.Millisecond
	roundTripSaveWait  = 5 * time.Second

	// drainStatusDeadline bounds the status call made during a drain.
	drainStatusDeadline = 3 * time.Second

	// rpcCallDeadline bounds a round trip call on a goroutine of its
	// own.
	rpcCallDeadline = 10 * time.Second

	// longLeaveTimeoutSeconds replaces the leave timeout of the live
	// configuration change.
	longLeaveTimeoutSeconds = 120

	// liveWebhookURL is the webhook of the live configuration change.
	liveWebhookURL = "https://discord.example.com/api/webhooks/1/test"

	// deliveredConfigIni is the config.ini content sent over the wire.
	deliveredConfigIni = "[path]\nwrite-data=/tmp/old\n"

	// changedConfigIni is the config.ini content of a restart.
	changedConfigIni = deliveredConfigIni + "max-players=5\n"

	// fakeGameVersionPrefix starts the line the fake game prints first,
	// followed by its version.
	fakeGameVersionPrefix = "   0.000 Factorio "

	// drainingLog is how the agent logs the entry into the drain.
	drainingLog = "phase=draining"

	// fakeGameStderrLine is the line the fake game prints on stderr.
	fakeGameStderrLine = "   2.000 Warning written to stderr"

	// fakeGameJoinLine is the player event the fake game prints.
	fakeGameJoinLine = "   1.000 [JOIN] tester joined the game"
)

// fakeFactorioScript returns the shell script standing in for the game
// binary of a version: --create writes the save, --start-server serves
// until it receives SIGINT or SIGTERM and then exits cleanly like the
// real game after saving. The trap is the first statement so an early
// stop still exits cleanly; the ready marker next to the save tells the
// tests that the server branch is running.
func fakeFactorioScript(version string) string {
	return `#!/bin/sh
trap 'exit 0' INT TERM
mode=""
save=""
while [ $# -gt 0 ]; do
  case "$1" in
    --create|--start-server) mode="$1"; save="$2" ;;
  esac
  shift
done
echo "   0.000 Factorio ` + version + `"
case "$mode" in
  --create)
    : > "$save"
    exit 0
    ;;
  --start-server)
    echo "` + fakeGameJoinLine + `"
    echo "` + fakeGameStderrLine + `" >&2
    : > "$save` + readyMarkerSuffix + `$$"
    while :; do sleep 0.05; done
    ;;
  *)
    echo "unexpected arguments: $*" >&2
    exit 1
    ;;
esac
`
}

// newRPCAgent wires a real agent behind a Cap'n Proto connection pair
// and returns the client capability.
func newRPCAgent(t *testing.T, log logr.Logger, options Options) (rpcapi.Agent, *Agent) {
	t.Helper()

	gameAgent, err := New(log, options)
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	serverEnd, clientEnd := net.Pipe()

	server := capnprpc.NewConn(
		capnprpc.NewStreamTransport(serverEnd),
		&capnprpc.Options{ //nolint:exhaustruct_v5 // Only the bootstrap capability matters.
			BootstrapClient: capnp.Client(rpcapi.Agent_ServerToClient(gameAgent)),
		},
	)

	t.Cleanup(func() { _ = server.Close() })

	client := capnprpc.NewConn(capnprpc.NewStreamTransport(clientEnd), nil)

	t.Cleanup(func() { _ = client.Close() })

	return rpcapi.Agent(client.Bootstrap(context.Background())), gameAgent
}

// plantFakeFactorio installs the fake game binary of a version.
func plantFakeFactorio(t *testing.T, cacheDir, version string) {
	t.Helper()

	binary := gameBinaryPath(cacheDir, version)

	err := os.MkdirAll(filepath.Dir(binary), directoryFileMode)
	if err != nil {
		t.Fatalf("could not create the binary directory: %v", err)
	}

	script := []byte(fakeFactorioScript(version))

	err = os.WriteFile(binary, script, 0o755) //nolint:gosec // The fake game binary must be executable.
	if err != nil {
		t.Fatalf("could not write the fake game binary: %v", err)
	}
}

// roundTripOptions returns the agent options of the RPC round trips.
func roundTripOptions(saveDir, cacheDir string, exec func(string, []string, []string) error) Options {
	return Options{ //nolint:exhaustruct_v5 // The defaults fill the optional fields.
		SaveDir:           saveDir,
		CacheDir:          cacheDir,
		DrainPollInterval: roundTripDrainPoll,
		SaveWaitTimeout:   roundTripSaveWait,
		ExecBinary:        exec,
	}
}

// roundTripConfig returns the configuration of the RPC round trips.
func roundTripConfig() *Config {
	return &Config{ //nolint:exhaustruct_v5 // The optional settings stay disabled.
		GameVersion:         testGameVersion,
		Files:               []ConfigFile{{Name: configFileIniName, Content: deliveredConfigIni}},
		LeaveTimeoutSeconds: 1,
	}
}

// gamePid returns the process id of the running game of the agent, or
// zero when it runs none.
func gamePid(gameAgent *Agent) int {
	gameAgent.mu.Lock()
	defer gameAgent.mu.Unlock()

	if gameAgent.game == nil {
		return 0
	}

	return gameAgent.game.Pid()
}

// runningGame returns the handle of the running game, or nil.
func runningGame(gameAgent *Agent) gameHandle { //nolint:ireturn // The tests only need the handle interface.
	gameAgent.mu.Lock()
	defer gameAgent.mu.Unlock()

	return gameAgent.game
}

// killGame stops the game behind the test agent, so no fake game
// process outlives the test.
func killGame(gameAgent *Agent) {
	game := runningGame(gameAgent)
	if game == nil {
		return
	}

	_ = game.Signal(syscall.SIGKILL)

	<-game.Exited()
}

// waitForReady polls for the ready marker of the fake game server, so a
// stop never races the script startup.
func waitForReady(t *testing.T, gameAgent *Agent) {
	t.Helper()

	pid := gamePid(gameAgent)
	if pid <= 0 {
		t.Fatalf("the agent runs no game to wait for (phase %s)", gameAgent.snapshot().phase)
	}

	marker := filepath.Join(gameAgent.savesDir(), saveFileName+readyMarkerSuffix+strconv.Itoa(pid))

	deadline := time.Now().Add(rpcCallDeadline)

	for {
		_, err := os.Stat(marker)
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("the fake game %d never became ready at %s", pid, marker)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// waitForPhase polls the status snapshot until it reports the phase.
func waitForPhase(t *testing.T, gameAgent *Agent, phase rpcapi.Phase) {
	t.Helper()

	deadline := time.Now().Add(rpcCallDeadline)

	for {
		if gameAgent.snapshot().phase == phase {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("phase = %s, want %s", gameAgent.snapshot().phase, phase)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// tryConfigure sends a full configuration over the wire without failing
// the test.
func tryConfigure(ctx context.Context, client rpcapi.Agent, config *Config) error {
	future, release := client.Configure(ctx, func(params rpcapi.Agent_configure_Params) error {
		return fillConfigureParams(params, config)
	})
	defer release()

	_, err := future.Struct()
	if err != nil {
		return err //nolint:wrapcheck // The raw RPC error is what the test inspects.
	}

	return nil
}

// callConfigure sends a full configuration over the wire.
func callConfigure(t *testing.T, client rpcapi.Agent, config *Config) {
	t.Helper()

	err := tryConfigure(t.Context(), client, config)
	if err != nil {
		t.Fatalf("configure call failed: %v", err)
	}
}

// callStatus reads the agent status over the wire.
func callStatus(t *testing.T, client rpcapi.Agent) gameStatus {
	t.Helper()

	status, err := statusOf(t.Context(), client)
	if err != nil {
		t.Fatalf("could not read the agent status: %v", err)
	}

	return status
}

// callStop stops the game over the wire and waits for the save.
func callStop(t *testing.T, client rpcapi.Agent) {
	t.Helper()

	err := stopOf(t.Context(), client)
	if err != nil {
		t.Fatalf("could not stop the game: %v", err)
	}
}

// fillConfigureParams writes the configuration into the call params.
func fillConfigureParams(params rpcapi.Agent_configure_Params, config *Config) error {
	out, err := params.NewCfg()
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = out.SetGameVersion(config.GameVersion)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = fillConfigureFiles(out, config)
	if err != nil {
		return err
	}

	err = out.SetDiscordWebhookURL(config.DiscordWebhookURL)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	out.SetLeaveTimeoutSeconds(config.LeaveTimeoutSeconds)

	err = fillConfigureBackup(out, config)
	if err != nil {
		return err
	}

	err = out.SetDownloadUsername(config.DownloadUsername)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	return out.SetDownloadToken(config.DownloadToken) //nolint:wrapcheck // The test reports the raw parameter error.
}

// fillConfigureFiles writes the delivered files into the params.
func fillConfigureFiles(out rpcapi.Config, config *Config) error {
	files, err := out.NewFiles(int32(len(config.Files))) //nolint:gosec // The test sends a handful of files.
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	for index, file := range config.Files {
		err = files.At(index).SetName(file.Name)
		if err != nil {
			return err //nolint:wrapcheck // The test reports the raw parameter error.
		}

		err = files.At(index).SetContent([]byte(file.Content))
		if err != nil {
			return err //nolint:wrapcheck // The test reports the raw parameter error.
		}
	}

	return nil
}

// fillConfigureBackup writes the backup settings into the params.
func fillConfigureBackup(out rpcapi.Config, config *Config) error {
	if config.Backup == nil {
		return nil
	}

	backup, err := out.NewBackup()
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = backup.SetEndpointURL(config.Backup.EndpointURL)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = backup.SetBucket(config.Backup.Bucket)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = backup.SetAccessKeyID(config.Backup.AccessKeyID)
	if err != nil {
		return err //nolint:wrapcheck // The test reports the raw parameter error.
	}

	err = backup.SetSecretAccessKey(config.Backup.SecretAccessKey)

	return err //nolint:wrapcheck // The test reports the raw parameter error.
}

// readFile reads a file, failing the test on errors.
func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // The path is built by the test itself.
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	return string(content)
}

// TestAgentConfigureStatusStopRoundTrip drives the full agent over a
// real Cap'n Proto RPC connection with a fake game binary.
func TestAgentConfigureStatusStopRoundTrip(t *testing.T) {
	t.Parallel()

	log, sink := newLogSink()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	client, gameAgent := newRPCAgent(t, log, roundTripOptions(t.TempDir(), cacheDir, nil))

	t.Cleanup(func() { killGame(gameAgent) })

	config := roundTripConfig()

	callConfigure(t, client, config)

	assertRoundTripStarted(t, client, gameAgent)

	assertGameOutputMirrored(t, sink)

	// A second configure with the same content must not restart the game.
	pid := gamePid(gameAgent)

	callConfigure(t, client, config)

	if pidAgain := gamePid(gameAgent); pidAgain != pid {
		t.Errorf("game pid after the idempotent configure = %d, want %d", pidAgain, pid)
	}

	waitForReady(t, gameAgent)

	callStop(t, client)

	status := callStatus(t, client)
	if status.phase != rpcapi.Phase_suspended {
		t.Errorf("phase after stop = %s, want suspended", status.phase)
	}

	if status.gameVersion != testGameVersion {
		t.Errorf("gameVersion after stop = %q, want %q", status.gameVersion, testGameVersion)
	}

	if status.agentVersion != version.Agent {
		t.Errorf("agentVersion after stop = %q, want %q", status.agentVersion, version.Agent)
	}

	if status.players != 0 {
		t.Errorf("players after stop = %d, want 0", status.players)
	}
}

// assertRoundTripStarted verifies the state after the first configure.
func assertRoundTripStarted(t *testing.T, client rpcapi.Agent, gameAgent *Agent) {
	t.Helper()

	status := callStatus(t, client)
	if status.phase != rpcapi.Phase_running {
		t.Errorf("phase after configure = %s, want running", status.phase)
	}

	if status.gameVersion != testGameVersion {
		t.Errorf("gameVersion after configure = %q, want %q", status.gameVersion, testGameVersion)
	}

	assertRoundTripFiles(t, gameAgent)

	if pid := gamePid(gameAgent); pid <= 0 {
		t.Fatal("the agent runs no game process")
	}
}

// gameVersionLog returns the logged line of a run of the game binary
// of a version. The agent logs the trimmed game output.
func gameVersionLog(version string) string {
	return "line=" + strings.TrimSpace(fakeGameVersionPrefix) + " " + version
}

// assertGameOutputMirrored verifies that the game output reached the
// agent log line by line, on both streams.
func assertGameOutputMirrored(t *testing.T, sink *logSink) {
	t.Helper()

	sink.waitForLog(t, "stream=stdout line="+strings.TrimSpace(fakeGameJoinLine))
	sink.waitForLog(t, "stream=stderr line="+strings.TrimSpace(fakeGameStderrLine))
}

// assertRoundTripFiles verifies the written and persisted files after a
// configure.
func assertRoundTripFiles(t *testing.T, gameAgent *Agent) {
	t.Helper()

	configIni := readFile(t, filepath.Join(gameAgent.saveDir, configFileIniName))
	persisted := readFile(t, filepath.Join(gameAgent.saveDir, stateFileName))

	wantIni := "[path]\nwrite-data=" + gameAgent.saveDir + "\n"
	if configIni != wantIni {
		t.Errorf("written config.ini = %q, want %q", configIni, wantIni)
	}

	if !strings.Contains(persisted, `"gameVersion": "`+testGameVersion+`"`) {
		t.Errorf("persisted state = %q, want the game version", persisted)
	}

	_, err := os.Stat(gameAgent.savePath())
	if err != nil {
		t.Errorf("could not find the created save: %v", err)
	}
}

// TestAgentConfigureAppliesLiveSettingsWithoutRestart verifies that a
// configuration which differs only in settings that need no restart
// applies to the running game: no drain, the same game process.
func TestAgentConfigureAppliesLiveSettingsWithoutRestart(t *testing.T) {
	t.Parallel()

	log, sink := newLogSink()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	client, gameAgent := newRPCAgent(t, log, roundTripOptions(t.TempDir(), cacheDir, nil))

	t.Cleanup(func() { killGame(gameAgent) })

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	pid := gamePid(gameAgent)

	live := roundTripConfig()
	live.DiscordWebhookURL = liveWebhookURL
	live.LeaveTimeoutSeconds = longLeaveTimeoutSeconds

	callConfigure(t, client, live)

	if got := gamePid(gameAgent); got != pid {
		t.Errorf("game pid after the live change = %d, want %d: the game must keep running", got, pid)
	}

	gameAgent.mu.Lock()
	applied, leaveTimeout := gameAgent.config, gameAgent.leaveTimeoutSeconds()
	gameAgent.mu.Unlock()

	if applied.DiscordWebhookURL != liveWebhookURL {
		t.Errorf("applied webhook = %q, want %q", applied.DiscordWebhookURL, liveWebhookURL)
	}

	if leaveTimeout != longLeaveTimeoutSeconds {
		t.Errorf("leave timeout = %d, want %d", leaveTimeout, longLeaveTimeoutSeconds)
	}

	if gameAgent.currentNotifier() == nil {
		t.Error("notifications stay disabled after the live change")
	}

	persisted := readFile(t, filepath.Join(gameAgent.saveDir, stateFileName))

	if !strings.Contains(persisted, liveWebhookURL) {
		t.Errorf("persisted state = %q, want the new webhook", persisted)
	}

	sink.assertNoLog(t, drainingLog)
}

// TestAgentConfigureRestartsOnAFileChange verifies that a changed
// config.ini drains and restarts the game.
func TestAgentConfigureRestartsOnAFileChange(t *testing.T) {
	t.Parallel()

	log, sink := newLogSink()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	client, gameAgent := newRPCAgent(t, log, roundTripOptions(t.TempDir(), cacheDir, nil))

	t.Cleanup(func() { killGame(gameAgent) })

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	old := runningGame(gameAgent)

	changed := roundTripConfig()
	changed.Files = []ConfigFile{{Name: configFileIniName, Content: changedConfigIni}}

	callConfigure(t, client, changed)

	select {
	case <-old.Exited():
	default:
		t.Error("the game of the previous configuration is still running")
	}

	if got := gamePid(gameAgent); got == 0 || got == old.Pid() {
		t.Errorf("game pid after the file change = %d, want a new process", got)
	}

	waitForReady(t, gameAgent)

	wantIni := "[path]\nwrite-data=" + gameAgent.saveDir + "\nmax-players=5\n"

	if configIni := readFile(t, filepath.Join(gameAgent.saveDir, configFileIniName)); configIni != wantIni {
		t.Errorf("written config.ini = %q, want %q", configIni, wantIni)
	}

	sink.waitForLog(t, drainingLog)
}

// TestAgentConfigureUpgradesTheGameVersion verifies that a game version
// change drains the running game, uses the binary of the new version
// and reports the new version when the call returns.
func TestAgentConfigureUpgradesTheGameVersion(t *testing.T) {
	t.Parallel()

	log, sink := newLogSink()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)
	plantFakeFactorio(t, cacheDir, testNewGameVersion)

	client, gameAgent := newRPCAgent(t, log, roundTripOptions(t.TempDir(), cacheDir, nil))

	t.Cleanup(func() { killGame(gameAgent) })

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	old := runningGame(gameAgent)

	// The server run of the old binary logged its version line.
	sink.waitForCount(t, gameVersionLog(testGameVersion), 1)

	upgraded := roundTripConfig()
	upgraded.GameVersion = testNewGameVersion

	callConfigure(t, client, upgraded)

	status := callStatus(t, client)
	if status.phase != rpcapi.Phase_running || status.gameVersion != testNewGameVersion {
		t.Errorf("status after the upgrade = %s %q, want running %q",
			status.phase, status.gameVersion, testNewGameVersion)
	}

	select {
	case <-old.Exited():
	default:
		t.Error("the game of the old version is still running")
	}

	waitForReady(t, gameAgent)

	// The output readers mirror the game output asynchronously.
	sink.waitForCount(t, gameVersionLog(testNewGameVersion), 1)

	if got := sink.count(gameVersionLog(testGameVersion)); got != 1 {
		t.Errorf("runs of the old game binary after the upgrade = %d, want 1", got)
	}
}

// TestAgentResumeFromCachedState verifies the pod restart flow: an
// agent that comes up on the volumes of a stopped pod starts the game
// from the cached configuration without any RPC call.
func TestAgentResumeFromCachedState(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	saveDir := t.TempDir()

	client, first := newRPCAgent(t, logr.Discard(), roundTripOptions(saveDir, cacheDir, nil))

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, first)

	callStop(t, client)

	// A new agent process comes up on the same volumes.
	log, _ := newLogSink()

	second, err := New(log, roundTripOptions(saveDir, cacheDir, nil))
	if err != nil {
		t.Fatalf("could not create the restarted agent: %v", err)
	}

	t.Cleanup(func() { killGame(second) })

	second.Resume(t.Context())

	waitForPhase(t, second, rpcapi.Phase_running)

	if pid := gamePid(second); pid <= 0 {
		t.Fatalf("the restarted agent runs no game: phase %s", second.snapshot().phase)
	}

	status := second.snapshot()
	if status.gameVersion != testGameVersion {
		t.Errorf("resumed gameVersion = %q, want %q", status.gameVersion, testGameVersion)
	}
}

// blockingRcon is an rconConn whose player queries block until it is
// released, like a game that needs a moment to answer.
type blockingRcon struct {
	release  chan struct{}
	unblocks sync.Once
}

// newBlockingRcon returns a blocking connection.
func newBlockingRcon() *blockingRcon {
	return &blockingRcon{release: make(chan struct{})} //nolint:exhaustruct_v5 // Starts locked.
}

// PlayersOnline waits for the release, then reports no player.
func (c *blockingRcon) PlayersOnline(_ context.Context) (int, error) {
	<-c.release

	return 0, nil
}

// Chat does nothing.
func (c *blockingRcon) Chat(_ context.Context, _ string) error { return nil }

// Close does nothing.
func (c *blockingRcon) Close() error { return nil }

// unblock releases the blocked queries, at least once.
func (c *blockingRcon) unblock() {
	c.unblocks.Do(func() { close(c.release) })
}

// TestAgentStatusAnswersDuringADrain verifies that a status call is
// answered while a stop call drains the game, and reports the drain.
func TestAgentStatusAnswersDuringADrain(t *testing.T) {
	t.Parallel()

	rcon := newBlockingRcon()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	client, gameAgent := newRPCAgent(t, logr.Discard(), Options{ //nolint:exhaustruct_v5 // Defaults apply.
		SaveDir:           t.TempDir(),
		CacheDir:          cacheDir,
		DialRcon:          func(_ string, _ string) (rconConn, error) { return rcon, nil },
		DrainPollInterval: roundTripDrainPoll,
		SaveWaitTimeout:   roundTripSaveWait,
	})

	t.Cleanup(func() {
		rcon.unblock()
		killGame(gameAgent)
	})

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	stopDone := make(chan error, 1)

	go func() { stopDone <- stopOf(context.Background(), client) }()

	waitForPhase(t, gameAgent, rpcapi.Phase_draining)

	ctx, cancel := context.WithTimeout(t.Context(), drainStatusDeadline)
	defer cancel()

	status, err := statusOf(ctx, client)
	if err != nil {
		t.Fatalf("a status call during the drain failed: %v", err)
	}

	if status.phase != rpcapi.Phase_draining {
		t.Errorf("phase during the drain = %s, want draining", status.phase)
	}

	rcon.unblock()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("the stop call failed: %v", err)
		}
	case <-time.After(rpcCallDeadline):
		t.Fatal("the stop call did not return")
	}

	assertSuspended(t, gameAgent)
}

// TestAgentReportsACrashedGame verifies that a game that exits on its
// own is reported as failed until the next configure restarts it.
func TestAgentReportsACrashedGame(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	client, gameAgent := newRPCAgent(t, logr.Discard(), roundTripOptions(t.TempDir(), cacheDir, nil))

	callConfigure(t, client, roundTripConfig())

	waitForReady(t, gameAgent)

	crashed := runningGame(gameAgent)

	_ = crashed.Signal(syscall.SIGKILL)

	<-crashed.Exited()

	waitForPhase(t, gameAgent, rpcapi.Phase_failed)

	status := callStatus(t, client)
	if status.phase != rpcapi.Phase_failed {
		t.Errorf("phase after the crash = %s, want failed", status.phase)
	}

	if status.players != 0 {
		t.Errorf("players after the crash = %d, want 0", status.players)
	}

	t.Cleanup(func() { killGame(gameAgent) })

	// The controller sends the full configuration again; that restarts the game.
	callConfigure(t, client, roundTripConfig())

	if pid := gamePid(gameAgent); pid <= 0 {
		t.Fatal("the agent runs no game after the configure that follows the crash")
	}

	if phase := callStatus(t, client).phase; phase != rpcapi.Phase_running {
		t.Errorf("phase after the restart = %s, want running", phase)
	}
}

// TestAgentShutdownRefusesAConfigure verifies the SIGTERM path: once
// the agent shuts down, a late configure or resume does not start the
// game again.
func TestAgentShutdownRefusesAConfigure(t *testing.T) {
	t.Parallel()

	conn := newFakeRcon(0)

	gameAgent, _ := newStopTestAgent(t, conn)

	err := gameAgent.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	assertSuspended(t, gameAgent)

	err = gameAgent.configure(t.Context(), roundTripConfig())
	if !errors.Is(err, errTerminating) {
		t.Errorf("configure() error = %v, want %v", err, errTerminating)
	}

	gameAgent.resumeConfiguration(t.Context(), roundTripConfig())

	if pid := gamePid(gameAgent); pid != 0 {
		t.Errorf("a game was started after the shutdown (pid %d)", pid)
	}
}
