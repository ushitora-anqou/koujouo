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

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// Graceful stop parameters. The drain warns the online players in
// Japanese, then waits for them to leave; the save wait bounds how long
// the game may take to write the save file after SIGINT before it is
// killed as a last resort.
const (
	defaultLeaveTimeoutSeconds = 60
	drainPollAttempts          = 3
	drainPollInterval          = 2 * time.Second
	defaultSaveWaitTimeout     = 5 * time.Minute
)

// errSaveTimeout marks a game that ignored SIGINT until it was killed.
var errSaveTimeout = errors.New("the game did not write the save in time")

// drainMessage builds the Japanese warning chat sent to the players
// before the server stops.
func drainMessage(leaveTimeoutSeconds int32) string {
	return fmt.Sprintf(
		"サーバーはまもなく停止します。%d秒以内に退出してください。", //nolint:gosmopolitan // The drain warning is Japanese by design.
		leaveTimeoutSeconds)
}

// stopLocked gracefully stops the running game: it drains the players,
// sends SIGINT (which makes Factorio write the save file and exit),
// waits for the exit, uploads one final backup and reports the
// suspended phase. It must be called with the lifecycle lock held.
func (a *Agent) stopLocked(ctx context.Context) error {
	game := a.game
	if game == nil {
		a.setPhaseLocked(rpcapi.Phase_suspended)

		return nil
	}

	a.setPhaseLocked(rpcapi.Phase_draining)
	a.drainGame(ctx)

	a.setPhaseLocked(rpcapi.Phase_saving)

	err := terminateGame(a.log, game, a.saveWaitTimeout)
	if err != nil {
		a.log.Error(err, "Could not stop Factorio cleanly")
	}

	a.stopLoopsLocked()
	a.backupAfterStop(ctx)

	a.game = nil
	a.setPlayersLocked(0)
	a.setPhaseLocked(rpcapi.Phase_suspended)

	return err
}

// drainGame connects to the RCON endpoint of the game and runs the
// drain. Connection failures skip the warning instead of blocking the
// stop.
func (a *Agent) drainGame(ctx context.Context) {
	if a.rconPort == 0 || a.rconPassword == "" {
		return
	}

	conn, err := a.dialRcon(rconAddress(a.rconPort), a.rconPassword)
	if err != nil {
		a.log.Error(err, "Could not connect to RCON for the drain")

		return
	}

	defer conn.Close() //nolint:errcheck // The drain connection needs no drain.

	a.drain(ctx, conn, a.leaveTimeoutSeconds())
}

// leaveTimeoutSeconds returns how long the drain waits for the players. A
// configured 0 waits for no player at all; only a missing configuration
// falls back to the default.
func (a *Agent) leaveTimeoutSeconds() int32 {
	if a.config != nil && a.config.LeaveTimeoutSeconds >= 0 {
		return a.config.LeaveTimeoutSeconds
	}

	return defaultLeaveTimeoutSeconds
}

// backupAfterStop uploads the save file one last time after a graceful
// stop. The upload gets its own bounded context: the stop context may
// have expired during a long drain.
func (a *Agent) backupAfterStop(ctx context.Context) {
	a.backupOnce(context.WithoutCancel(ctx))
}

// drain warns the online players and waits until they left or the leave
// timeout passed. It never blocks the stop: RCON failures skip the
// warning instead of waiting.
func (a *Agent) drain(ctx context.Context, conn rconConn, leaveTimeoutSeconds int32) {
	players, err := queryPlayers(ctx, conn, a.drainPollInterval)
	if err != nil {
		a.log.Error(err, "Could not query the online players, stopping without a warning")

		return
	}

	if players <= 0 {
		a.log.Info("No players are online")

		return
	}

	a.log.Info("Warning the online players", "players", players)

	err = conn.Chat(ctx, drainMessage(leaveTimeoutSeconds))
	if err != nil {
		a.log.Error(err, "Could not send the drain warning")
	}

	a.waitForPlayers(ctx, conn, time.Duration(leaveTimeoutSeconds)*time.Second)
}

// waitForPlayers polls the player count until everyone left, the leave
// timeout passed or ctx is done.
func (a *Agent) waitForPlayers(ctx context.Context, conn rconConn, leaveTimeout time.Duration) {
	deadline := time.NewTimer(leaveTimeout)
	defer deadline.Stop()

	ticker := time.NewTicker(a.drainPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.log.Info("Drain context ended", "reason", ctx.Err().Error())

			return
		case <-deadline.C:
			a.log.Info("Leave timeout reached", "timeout", leaveTimeout.String())

			return
		case <-ticker.C:
			players, err := conn.PlayersOnline(ctx)
			if err != nil {
				a.log.Error(err, "Could not query the online players during the drain")

				continue
			}

			if players <= 0 {
				a.log.Info("All players left the game")

				return
			}
		}
	}
}

// queryPlayers reads the player count, retrying a few times because the
// RCON endpoint may not answer immediately after the game started.
func queryPlayers(ctx context.Context, conn rconConn, interval time.Duration) (int, error) {
	var lastErr error

	for range drainPollAttempts {
		players, err := conn.PlayersOnline(ctx)
		if err == nil {
			return players, nil
		}

		lastErr = err

		sleepErr := sleepContext(ctx, interval)
		if sleepErr != nil {
			return 0, sleepErr
		}
	}

	return 0, lastErr
}

// terminateGame sends SIGINT to the game, which makes Factorio write
// the save file and exit, and waits for the exit. A game that does not
// exit within the save wait timeout is killed.
func terminateGame(log logr.Logger, game gameHandle, timeout time.Duration) error {
	err := game.Signal(syscall.SIGINT)
	if err != nil {
		return fmt.Errorf("could not send SIGINT to the game: %w", err)
	}

	select {
	case <-game.Exited():
		return exitAfterInterrupt(log, game.Err())
	case <-time.After(timeout):
		err = game.Signal(syscall.SIGKILL)
		if err != nil {
			return fmt.Errorf("could not kill the game after the save timeout: %w", err)
		}

		<-game.Exited()

		return fmt.Errorf("the game did not exit within %s and was killed: %w", timeout, errSaveTimeout)
	}
}

// exitAfterInterrupt classifies the wait result of a game that was
// asked to stop with SIGINT: a process that exited on its own handled
// the signal and wrote the save, one that died from the signal did not.
func exitAfterInterrupt(log logr.Logger, err error) error {
	if err == nil {
		return nil
	}

	exitErr, isExitError := errors.AsType[*exec.ExitError](err)
	if isExitError && exitErr.ProcessState != nil && !exitErr.Exited() {
		return fmt.Errorf("the game died from a signal instead of saving: %w", err)
	}

	log.Info("Factorio exited with a non-zero status after SIGINT", "result", err.Error())

	return nil
}
