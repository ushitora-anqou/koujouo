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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// Environment variables carrying the running game across the execve(2) of
// an agent upgrade. The file descriptors of the game output pipes stay
// valid because their close-on-exec flag is cleared before the exec.
const (
	envFactorioPid      = "KOUJOUO_FACTORIO_PID"
	envFactorioStdoutFd = "KOUJOUO_FACTORIO_STDOUT_FD"
	envFactorioStderrFd = "KOUJOUO_FACTORIO_STDERR_FD"
)

// Upgrade file layout. The received binary first lands on the temporary
// suffix, so a failed stream never leaves a half-written executable.
const (
	agentNextName     = "agent.next"
	agentTempSuffix   = ".tmp"
	binaryFileMode    = 0o755
	sha256LengthBytes = sha256.Size
)

// errUpgradeVerify marks a failed verification of the received binary.
var errUpgradeVerify = errors.New("the received agent binary is invalid")

// errUpgradeOutput marks a game output stream that cannot survive the
// exec of an upgrade.
var errUpgradeOutput = errors.New("the game output cannot be passed across the exec")

// binReceiver implements the ByteStream server of an upgrade: it
// collects the chunks of the new agent binary and execve(2)s it once the
// stream ends with a matching sha256.
type binReceiver struct {
	agent *Agent

	path  string
	file  *os.File
	hash  hash.Hash
	mutex sync.Mutex
}

// newBinReceiver opens the temporary target file of a new binary.
func newBinReceiver(agent *Agent) (*binReceiver, error) {
	err := os.MkdirAll(agent.cacheDir, directoryFileMode)
	if err != nil {
		return nil, fmt.Errorf("could not create the cache directory: %w", err)
	}

	path := filepath.Join(agent.cacheDir, agentNextName+agentTempSuffix)

	file, err := os.OpenFile(path, //nolint:gosec // The path is built by the agent itself.
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, binaryFileMode)
	if err != nil {
		return nil, fmt.Errorf("could not create the agent binary file %s: %w", path, err)
	}

	return &binReceiver{
		agent: agent,
		path:  path,
		file:  file,
		hash:  sha256.New(),
		mutex: sync.Mutex{},
	}, nil
}

// Write appends one chunk of the new binary.
func (r *binReceiver) Write(_ context.Context, call rpcapi.ByteStream_write) error {
	chunk, err := call.Args().Chunk()
	if err != nil {
		return fmt.Errorf("could not read the chunk of the agent binary: %w", err)
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	_, err = r.file.Write(chunk)
	if err != nil {
		return fmt.Errorf("could not write the agent binary %s: %w", r.path, err)
	}

	_, err = r.hash.Write(chunk)
	if err != nil {
		return fmt.Errorf("could not hash the agent binary %s: %w", r.path, err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the write results: %w", err)
	}

	return nil
}

// Done verifies the sha256 of the received binary, installs it and
// execve(2)s it. On success the call never returns, because the whole
// agent process is replaced.
func (r *binReceiver) Done(_ context.Context, call rpcapi.ByteStream_done) error {
	sum, err := call.Args().Sha256()
	if err != nil {
		return fmt.Errorf("could not read the sha256 of the agent binary: %w", err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the done results: %w", err)
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	err = r.finish(sum)
	if err != nil {
		// Best effort: a no-op once the binary was installed.
		_ = os.Remove(r.path)
	}

	return err
}

// finish installs and execs the new binary. It must be called with the
// receiver mutex held.
func (r *binReceiver) finish(sum []byte) error {
	next, err := r.install(sum)
	if err != nil {
		return err
	}

	r.agent.log.Info("Execing the new agent binary", "path", next)

	return r.agent.execInto(next)
}

// install verifies the received binary, seals it and moves it to the path
// the next agent process runs. It must be called with the receiver mutex
// held. The file is closed before it returns: execve(2) refuses a binary
// that a process keeps open for writing (ETXTBSY).
func (r *binReceiver) install(sum []byte) (string, error) {
	sealErr := r.seal(sum)
	closeErr := r.file.Close()

	if sealErr != nil {
		return "", sealErr
	}

	if closeErr != nil {
		return "", fmt.Errorf("could not close the agent binary %s: %w", r.path, closeErr)
	}

	next := filepath.Join(r.agent.cacheDir, agentNextName)

	err := os.Rename(r.path, next)
	if err != nil {
		return "", fmt.Errorf("could not install the agent binary %s: %w", next, err)
	}

	return next, nil
}

// seal verifies the sha256 of the received binary, makes it executable and
// flushes it to the disk.
func (r *binReceiver) seal(sum []byte) error {
	if len(sum) != sha256LengthBytes {
		return fmt.Errorf("could not verify the agent binary: sha256 must be %d bytes, got %d: %w",
			sha256LengthBytes, len(sum), errUpgradeVerify)
	}

	received := r.hash.Sum(nil)
	if !bytes.Equal(received, sum) {
		return fmt.Errorf("could not verify the agent binary: sha256 mismatch, expected %s, got %s: %w",
			hex.EncodeToString(sum), hex.EncodeToString(received), errUpgradeVerify)
	}

	// fchmod(2) is not subject to the umask, so the new agent binary
	// is executable whoever started this agent.
	err := r.file.Chmod(binaryFileMode)
	if err != nil {
		return fmt.Errorf("could not make the agent binary %s executable: %w", r.path, err)
	}

	err = r.file.Sync()
	if err != nil {
		return fmt.Errorf("could not sync the agent binary %s: %w", r.path, err)
	}

	return nil
}

// execInto replaces this agent process with the binary at path. The
// running game keeps running: its pid and output file descriptors
// travel in the environment, so the next agent process re-attaches to
// it. A game that already exited is not passed on: the next agent
// process then resumes from the cached state instead.
func (a *Agent) execInto(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	environment := dropEnv(os.Environ(), envFactorioPid, envFactorioStdoutFd, envFactorioStderrFd)

	err := a.persistStateLocked()
	if err != nil {
		return err
	}

	environment, err = a.appendGameEnv(environment)
	if err != nil {
		return err
	}

	err = a.execBinary(path, os.Args, environment)
	if err != nil {
		return fmt.Errorf("could not exec %s: %w", path, err)
	}

	return nil
}

// appendGameEnv appends the variables that carry the running game
// across the execve(2) of an upgrade. It must be called with the
// lifecycle lock held.
func (a *Agent) appendGameEnv(environment []string) ([]string, error) {
	game := a.game
	if game == nil {
		return environment, nil
	}

	select {
	case <-game.Exited():
		a.log.Info("The game already exited, the new agent resumes from the cached state")

		return environment, nil
	default:
	}

	err := clearCloexecFd(game)
	if err != nil {
		return nil, err
	}

	return append(environment,
		envFactorioPid+"="+strconv.Itoa(game.Pid()),
		envFactorioStdoutFd+"="+strconv.Itoa(int(game.StdoutFd())),
		envFactorioStderrFd+"="+strconv.Itoa(int(game.StderrFd())),
	), nil
}

// clearCloexecFd clears the close-on-exec flags of the game output
// pipes so the next agent process can keep reading them.
func clearCloexecFd(game gameHandle) error {
	stdout, stdoutIsFile := game.Stdout().(*os.File)
	if !stdoutIsFile {
		return fmt.Errorf("could not pass the game stdout across the exec of %T: %w",
			game.Stdout(), errUpgradeOutput)
	}

	stderr, stderrIsFile := game.Stderr().(*os.File)
	if !stderrIsFile {
		return fmt.Errorf("could not pass the game stderr across the exec of %T: %w",
			game.Stderr(), errUpgradeOutput)
	}

	err := clearCloexec(stdout)
	if err != nil {
		return fmt.Errorf("could not keep the game stdout across the exec: %w", err)
	}

	err = clearCloexec(stderr)
	if err != nil {
		return fmt.Errorf("could not keep the game stderr across the exec: %w", err)
	}

	return nil
}

// dropEnv removes the given variables from an environment slice.
func dropEnv(environment []string, names ...string) []string {
	kept := make([]string, 0, len(environment))

	for _, entry := range environment {
		key, _, _ := cutEnv(entry)

		dropped := slices.Contains(names, key)

		if !dropped {
			kept = append(kept, entry)
		}
	}

	return kept
}

// cutEnv splits one environment entry into its key and value.
func cutEnv(entry string) (string, string, bool) {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}

	return entry, "", false
}
