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
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"

	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// Fixtures of the graceful stop tests.
const (
	// testPid is the process id of the fake game.
	testPid = 4242

	// stopTestLeaveTimeoutSeconds is the leave timeout of the stop tests.
	stopTestLeaveTimeoutSeconds = 1

	// stopTestDrainPoll is the drain poll interval of the stop tests.
	stopTestDrainPoll = 10 * time.Millisecond

	// stopTestSaveWait is the save wait timeout of the stop tests.
	stopTestSaveWait = 2 * time.Second

	// killTimeout is the save wait timeout of the SIGKILL test.
	killTimeout = 50 * time.Millisecond

	// exitStatusCommand exits with a non-zero status.
	exitStatusCommand = "exit 3"

	// signalDeathCommand dies from a signal without exiting on its own.
	signalDeathCommand = "kill -TERM $$"
)

// fakeGame is a gameHandle that records the signals and exits when
// asked to.
type fakeGame struct {
	pid     int
	exited  chan struct{}
	err     error
	exitMu  sync.Mutex
	signals []syscall.Signal
}

// newFakeGame returns a running fake game.
func newFakeGame() *fakeGame {
	return &fakeGame{
		pid:    testPid,
		exited: make(chan struct{}),
	}
}

// Pid returns the fake process id.
func (f *fakeGame) Pid() int { return f.pid }

// StdoutFd returns a fake descriptor.
func (f *fakeGame) StdoutFd() uintptr { return 0 }

// StderrFd returns a fake descriptor.
func (f *fakeGame) StderrFd() uintptr { return 0 }

// Stdout returns an empty stream.
func (f *fakeGame) Stdout() io.Reader { return strings.NewReader("") }

// Stderr returns an empty stream.
func (f *fakeGame) Stderr() io.Reader { return strings.NewReader("") }

// Signal records the signal; SIGINT and SIGKILL end the fake process.
func (f *fakeGame) Signal(sig syscall.Signal) error {
	f.exitMu.Lock()
	f.signals = append(f.signals, sig)
	f.exitMu.Unlock()

	if sig == syscall.SIGINT || sig == syscall.SIGKILL {
		f.exit()
	}

	return nil
}

// Exited is closed once the fake process exited.
func (f *fakeGame) Exited() <-chan struct{} { return f.exited }

// Err returns the fake wait result.
func (f *fakeGame) Err() error {
	<-f.exited

	return f.err
}

// exit simulates the process exit.
func (f *fakeGame) exit() {
	f.exitMu.Lock()
	defer f.exitMu.Unlock()

	select {
	case <-f.exited:
	default:
		close(f.exited)
	}
}

// recordedSignals returns the received signals.
func (f *fakeGame) recordedSignals() []syscall.Signal {
	f.exitMu.Lock()
	defer f.exitMu.Unlock()

	return append([]syscall.Signal{}, f.signals...)
}

// fakeRcon is an rconConn with scriptable responses.
type fakeRcon struct {
	mutex   sync.Mutex
	players []int
	err     error
	chats   []string
}

// newFakeRcon returns a connection answering with the given player
// counts, one per query (the last one repeats).
func newFakeRcon(players ...int) *fakeRcon {
	return &fakeRcon{
		players: players,
	}
}

// PlayersOnline returns the scripted player count.
func (f *fakeRcon) PlayersOnline(_ context.Context) (int, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	if len(f.players) > 1 {
		count := f.players[0]
		f.players = f.players[1:]

		return count, nil
	}

	if len(f.players) == 1 {
		return f.players[0], nil
	}

	return 0, nil
}

// Chat records the message.
func (f *fakeRcon) Chat(_ context.Context, message string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.chats = append(f.chats, message)

	return nil
}

// Close closes the connection.
func (f *fakeRcon) Close() error { return nil }

// recordedChats returns the sent chat messages.
func (f *fakeRcon) recordedChats() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return append([]string{}, f.chats...)
}

// newStopTestAgent returns an agent running a fake game and wired to
// the fake RCON connection conn.
func newStopTestAgent(t *testing.T, conn rconConn) (*Agent, *fakeGame) {
	t.Helper()

	gameAgent, err := New(logr.Discard(), Options{
		SaveDir:           t.TempDir(),
		CacheDir:          t.TempDir(),
		DrainPollInterval: stopTestDrainPoll,
		SaveWaitTimeout:   stopTestSaveWait,
	})
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	game := newFakeGame()

	gameAgent.mu.Lock()
	gameAgent.config = &Config{
		GameVersion:         testGameVersion,
		LeaveTimeoutSeconds: stopTestLeaveTimeoutSeconds,
	}
	gameAgent.game = game
	gameAgent.rconPort = rconDefaultPort
	gameAgent.rconPassword = testRconPassword
	gameAgent.phase = rpcapi.Phase_running
	gameAgent.mu.Unlock()

	gameAgent.dialRcon = func(_ string, _ string) (rconConn, error) { return conn, nil }

	return gameAgent, game
}

// stopAgent stops the game of the test agent and returns how long the
// stop took.
func stopAgent(t *testing.T, gameAgent *Agent) time.Duration {
	t.Helper()

	start := time.Now()

	err := gameAgent.stop(t.Context())
	if err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	return time.Since(start)
}

// assertSignals verifies the signals the fake game received.
func assertSignals(t *testing.T, game *fakeGame, want ...syscall.Signal) {
	t.Helper()

	signals := game.recordedSignals()

	if len(signals) != len(want) {
		t.Errorf("signals = %v, want %v", signals, want)

		return
	}

	for index, signal := range want {
		if signals[index] != signal {
			t.Errorf("signals = %v, want %v", signals, want)

			return
		}
	}
}

// TestStopWithoutPlayers verifies that the stop sends SIGINT
// immediately when no player is online.
func TestStopWithoutPlayers(t *testing.T) {
	t.Parallel()

	conn := newFakeRcon(0)

	gameAgent, game := newStopTestAgent(t, conn)

	elapsed := stopAgent(t, gameAgent)

	if elapsed >= stopTestLeaveTimeoutSeconds*time.Second {
		t.Errorf("stop() took %v, want no wait without players", elapsed)
	}

	assertSignals(t, game, syscall.SIGINT)

	if chats := conn.recordedChats(); len(chats) != 0 {
		t.Errorf("chats = %v, want none", chats)
	}

	assertSuspended(t, gameAgent)
}

// TestStopWaitsForPlayersToLeave verifies the warning chat and the
// early SIGINT once everyone left.
func TestStopWaitsForPlayersToLeave(t *testing.T) {
	t.Parallel()

	// Two polls still see players, the third one sees none.
	conn := newFakeRcon(2, 2, 0)

	gameAgent, game := newStopTestAgent(t, conn)

	elapsed := stopAgent(t, gameAgent)

	if elapsed >= stopTestLeaveTimeoutSeconds*time.Second {
		t.Errorf("stop() took %v, want the SIGINT as soon as everyone left", elapsed)
	}

	chats := conn.recordedChats()

	wantWarning := drainMessage(stopTestLeaveTimeoutSeconds)

	if len(chats) != 1 || chats[0] != wantWarning {
		t.Errorf("chats = %q, want one drain warning %q", chats, wantWarning)
	}

	assertSignals(t, game, syscall.SIGINT)

	assertSuspended(t, gameAgent)
}

// TestStopTimesOut verifies that the players are forced out after the
// leave timeout.
func TestStopTimesOut(t *testing.T) {
	t.Parallel()

	conn := newFakeRcon(3)

	gameAgent, game := newStopTestAgent(t, conn)

	elapsed := stopAgent(t, gameAgent)

	if elapsed < stopTestLeaveTimeoutSeconds*time.Second {
		t.Errorf("stop() returned after %v, want at least the leave timeout", elapsed)
	}

	assertSignals(t, game, syscall.SIGINT)

	assertSuspended(t, gameAgent)
}

// TestStopWithZeroLeaveTimeout verifies that a configured leave timeout of
// 0 means "wait for no player at all" instead of falling back to the
// default.
func TestStopWithZeroLeaveTimeout(t *testing.T) {
	t.Parallel()

	conn := newFakeRcon(3)

	gameAgent, game := newStopTestAgent(t, conn)

	gameAgent.mu.Lock()
	gameAgent.config.LeaveTimeoutSeconds = 0
	gameAgent.mu.Unlock()

	elapsed := stopAgent(t, gameAgent)

	if elapsed >= stopTestLeaveTimeoutSeconds*time.Second {
		t.Errorf("stop() took %v, want no wait for a leave timeout of 0", elapsed)
	}

	assertSignals(t, game, syscall.SIGINT)

	if chats := conn.recordedChats(); len(chats) != 1 {
		t.Errorf("chats = %q, want one drain warning", chats)
	}

	assertSuspended(t, gameAgent)
}

// TestLeaveTimeoutSecondsDefaults verifies the fallback: only a missing
// configuration yields the default, a configured value is reported as it
// is.
func TestLeaveTimeoutSecondsDefaults(t *testing.T) {
	t.Parallel()

	gameAgent, err := New(logr.Discard(), Options{
		SaveDir:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	if got := gameAgent.leaveTimeoutSeconds(); got != defaultLeaveTimeoutSeconds {
		t.Errorf("leaveTimeoutSeconds() = %d, want %d without a configuration", got, defaultLeaveTimeoutSeconds)
	}

	gameAgent.config = &Config{LeaveTimeoutSeconds: 0}

	if got := gameAgent.leaveTimeoutSeconds(); got != 0 {
		t.Errorf("leaveTimeoutSeconds() = %d, want 0", got)
	}
}

// TestStopWithoutRcon verifies that a stop whose RCON endpoint does
// not answer still stops the game without a warning.
func TestStopWithoutRcon(t *testing.T) {
	t.Parallel()

	conn := &fakeRcon{
		err: errRconDown,
	}

	gameAgent, game := newStopTestAgent(t, conn)

	start := time.Now()

	err := gameAgent.stop(t.Context())
	if err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	elapsed := time.Since(start)

	if elapsed >= stopTestLeaveTimeoutSeconds*time.Second {
		t.Errorf("stop() took %v, want no wait when the player count is unknown", elapsed)
	}

	assertSignals(t, game, syscall.SIGINT)

	if chats := conn.recordedChats(); len(chats) != 0 {
		t.Errorf("chats = %v, want none", chats)
	}
}

// assertSuspended verifies that the agent reports the suspended phase
// and runs no game anymore.
func assertSuspended(t *testing.T, gameAgent *Agent) {
	t.Helper()

	gameAgent.mu.Lock()
	phase, gameRunning := gameAgent.phase, gameAgent.game != nil
	gameAgent.mu.Unlock()

	if phase != rpcapi.Phase_suspended {
		t.Errorf("phase = %s, want suspended", phase)
	}

	if gameRunning {
		t.Error("the game handle was not cleared")
	}
}

// TestStopWithoutGame verifies that stopping a stopped agent is a
// no-op that still reports the suspended phase.
func TestStopWithoutGame(t *testing.T) {
	t.Parallel()

	gameAgent, err := New(logr.Discard(), Options{
		SaveDir:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	err = gameAgent.stop(t.Context())
	if err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	assertSuspended(t, gameAgent)
}

// errRconDown stands in for an RCON endpoint that does not answer.
var errRconDown = errors.New("rcon is down")

// hangingGame is a fake game that ignores SIGINT and dies only on
// SIGKILL.
type hangingGame struct {
	*fakeGame
}

// Signal records the signal; only SIGKILL ends the hanging game.
func (h *hangingGame) Signal(sig syscall.Signal) error {
	h.exitMu.Lock()
	h.signals = append(h.signals, sig)
	h.exitMu.Unlock()

	if sig == syscall.SIGKILL {
		h.exit()
	}

	return nil
}

// TestTerminateGameKillsAfterTimeout verifies the last-resort SIGKILL
// of a game that ignores SIGINT.
func TestTerminateGameKillsAfterTimeout(t *testing.T) {
	t.Parallel()

	hanging := &hangingGame{fakeGame: newFakeGame()}

	err := terminateGame(logr.Discard(), hanging, killTimeout)
	if err == nil {
		t.Fatal("terminateGame() error = nil, want an error")
	}

	assertSignals(t, hanging.fakeGame, syscall.SIGINT, syscall.SIGKILL)
}

// TestExitAfterInterrupt verifies how the wait result of a game asked
// to stop is classified: a non-zero exit still saved, a death from the
// signal did not.
func TestExitAfterInterrupt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		err   func() error
		saved bool
	}{
		{name: "no error", err: func() error { return nil }, saved: true},
		{name: "non-zero exit", err: func() error { return waitResult(t, exitStatusCommand) }, saved: true},
		{name: "death from the signal", err: func() error { return waitResult(t, signalDeathCommand) }, saved: false},
	}

	for _, test := range tests {
		err := exitAfterInterrupt(logr.Discard(), test.err())
		if saved := err == nil; saved != test.saved {
			t.Errorf("%s: exitAfterInterrupt() error = %v, want saved = %v", test.name, err, test.saved)
		}
	}
}

// waitResult runs a shell command and returns its wait result, which is
// a real *exec.ExitError for the failing commands.
func waitResult(t *testing.T, command string) error {
	t.Helper()

	err := exec.Command("sh", "-c", command).Run() //nolint:noctx // Local shell run.

	if _, isExitError := errors.AsType[*exec.ExitError](err); !isExitError {
		t.Fatalf("could not obtain an exit error from %q: %v", command, err)
	}

	return err
}
