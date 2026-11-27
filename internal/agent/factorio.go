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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// saveFileName is the canonical save file the agent runs. It lives in
// the saves directory under the save volume.
const saveFileName = "map.zip"

// firstInheritedFd is the lowest descriptor the agent adopts from the
// previous agent process: the standard streams are never game output.
const firstInheritedFd = 3

// errGameGone marks a game of the previous agent process that is not
// running anymore.
var errGameGone = errors.New("the game of the previous agent process is gone")

// gameHandle is the running Factorio process from the agent's point of
// view. It is an interface so the graceful stop logic can be tested
// with a fake process.
type gameHandle interface {
	// Pid returns the process id of the game.
	Pid() int

	// StdoutFd returns the file descriptor of the game stdout pipe.
	// The agent passes it across execve(2) when upgrading itself.
	StdoutFd() uintptr

	// StderrFd returns the file descriptor of the game stderr pipe.
	StderrFd() uintptr

	// Stdout returns the readable end of the game stdout pipe.
	Stdout() io.Reader

	// Stderr returns the readable end of the game stderr pipe.
	Stderr() io.Reader

	// Signal sends a signal to the game process.
	Signal(sig syscall.Signal) error

	// Exited is closed once the game process has exited and been
	// reaped; afterwards Err holds its wait result.
	Exited() <-chan struct{}

	// Err returns the wait result of the game process. It is valid
	// only after Exited is closed.
	Err() error
}

// gameProcess is the production gameHandle built on os/exec.
type gameProcess struct {
	process *os.Process
	stdout  *os.File
	stderr  *os.File
	exited  chan struct{}
	err     error
}

// waiter is the waitable representation of the child: the exec.Cmd of a
// started process, or the os.Process of a reattached one. Wait reports
// a non-zero exit as an *exec.ExitError, like exec.Cmd.Wait does.
type waiter interface {
	Wait() error
}

// processWaiter adapts an os.Process (of a reattached game) to waiter.
type processWaiter struct {
	process *os.Process
}

// Wait reaps the process once it exits.
func (w processWaiter) Wait() error {
	state, err := w.process.Wait()
	if err != nil {
		return fmt.Errorf("could not wait for the Factorio process %d: %w", w.process.Pid, err)
	}

	if state.Success() {
		return nil
	}

	return &exec.ExitError{ProcessState: state, Stderr: nil}
}

// startParams carries the arguments of one game process run.
type startParams struct {
	binary        string
	configPath    string
	savePath      string
	whitelistPath string
	rconPort      int
	rconPassword  string
	create        bool
}

// factorioArgs assembles the game command line. A create run omits the
// RCON arguments because it exits as soon as the save is written.
func factorioArgs(params startParams) []string {
	args := []string{"-c", params.configPath}

	if params.whitelistPath != "" {
		args = append(args, "--use-server-whitelist", "--server-whitelist", params.whitelistPath)
	}

	if params.create {
		return append(args, "--create", params.savePath)
	}

	args = append(args, "--start-server", params.savePath,
		"--rcon-bind", rconAddress(params.rconPort),
		"--rcon-password", params.rconPassword)

	return args
}

// startGameProcess starts the game binary with the given run
// parameters and returns its handle. The caller becomes responsible
// for reading Stdout and Stderr. The game outlives the call that
// started it, so it is deliberately not bound to a context.
func startGameProcess(workingDir string, params startParams) (*gameProcess, error) {
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("could not create the game stdout pipe: %w", err)
	}

	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		closePipe(stdout, stdoutWriter)

		return nil, fmt.Errorf("could not create the game stderr pipe: %w", err)
	}

	game, err := runGame(workingDir, params, stdout, stdoutWriter, stderr, stderrWriter)
	if err != nil {
		closePipe(stdout, stdoutWriter)
		closePipe(stderr, stderrWriter)

		return nil, err
	}

	return game, nil
}

// runGame starts the game process with its output wired to the pipes.
func runGame(
	workingDir string,
	params startParams,
	stdout, stdoutWriter, stderr, stderrWriter *os.File,
) (*gameProcess, error) {
	args := factorioArgs(params)

	command := exec.Command(params.binary, args...) //nolint:gosec,noctx // The game outlives any call context.
	command.Dir = workingDir
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter

	err := command.Start()
	if err != nil {
		return nil, fmt.Errorf("could not start Factorio %s: %w", params.binary, err)
	}

	// The parent must close its write ends, or the readers never see
	// EOF after the game exits.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	game := &gameProcess{
		process: command.Process,
		stdout:  stdout,
		stderr:  stderr,
		exited:  make(chan struct{}),
		err:     nil,
	}

	go game.reap(command)

	return game, nil
}

// closePipe closes both ends of a pipe that is not used after all.
func closePipe(readEnd, writeEnd *os.File) {
	_ = readEnd.Close()
	_ = writeEnd.Close()
}

// reattachGameProcess adopts the game process left behind by the
// previous agent process after an upgrade execve(2). The process is still
// our child, so waitpid(2) keeps working, and the output pipes travel
// in the inherited file descriptors.
func reattachGameProcess(pid int, stdoutFd, stderrFd uintptr) (*gameProcess, error) {
	err := reapIfExited(pid)
	if err != nil {
		return nil, err
	}

	stdout, err := adoptOutputFd(stdoutFd, "factorio-stdout")
	if err != nil {
		return nil, err
	}

	stderr, err := adoptOutputFd(stderrFd, "factorio-stderr")
	if err != nil {
		_ = stdout.Close()

		return nil, err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()

		return nil, fmt.Errorf("could not find the Factorio process %d: %w", pid, err)
	}

	game := &gameProcess{
		process: process,
		stdout:  stdout,
		stderr:  stderr,
		exited:  make(chan struct{}),
		err:     nil,
	}

	go game.reap(processWaiter{process: process})

	return game, nil
}

// reapIfExited verifies that the game of pid is a live child of this
// process. A child that already exited is reaped and reported as gone;
// a pid that is not a child of this process is reported as gone too.
func reapIfExited(pid int) error {
	var status syscall.WaitStatus

	waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err != nil {
		return fmt.Errorf("could not adopt the Factorio process %d, it is not a child of this agent: %w", pid, err)
	}

	if waited != 0 {
		return fmt.Errorf("could not adopt the Factorio process %d, it already exited (%s): %w",
			pid, describeWaitStatus(status), errGameGone)
	}

	return nil
}

// describeWaitStatus renders the exit status of a reaped process.
func describeWaitStatus(status syscall.WaitStatus) string {
	if status.Exited() {
		return "exit status " + strconv.Itoa(status.ExitStatus())
	}

	if status.Signaled() {
		return "signal " + status.Signal().String()
	}

	return "unknown status"
}

// adoptOutputFd wraps an inherited game output descriptor.
func adoptOutputFd(descriptor uintptr, name string) (*os.File, error) {
	file := os.NewFile(descriptor, name)
	if file == nil {
		return nil, fmt.Errorf("could not adopt the %s descriptor %d of the game: %w",
			name, descriptor, errGameGone)
	}

	_, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("could not adopt the %s descriptor %d of the game: %w", name, descriptor, err)
	}

	return file, nil
}

// Pid returns the process id of the game.
func (g *gameProcess) Pid() int {
	return g.process.Pid
}

// StdoutFd returns the file descriptor of the game stdout pipe.
func (g *gameProcess) StdoutFd() uintptr {
	return g.stdout.Fd()
}

// StderrFd returns the file descriptor of the game stderr pipe.
func (g *gameProcess) StderrFd() uintptr {
	return g.stderr.Fd()
}

// Stdout returns the readable end of the game stdout pipe.
func (g *gameProcess) Stdout() io.Reader {
	return g.stdout
}

// Stderr returns the readable end of the game stderr pipe.
func (g *gameProcess) Stderr() io.Reader {
	return g.stderr
}

// Signal sends a signal to the game process.
func (g *gameProcess) Signal(sig syscall.Signal) error {
	err := g.process.Signal(sig)
	if err != nil {
		return fmt.Errorf("could not signal the Factorio process %d: %w", g.process.Pid, err)
	}

	return nil
}

// Exited is closed once the game process has exited and been reaped.
func (g *gameProcess) Exited() <-chan struct{} {
	return g.exited
}

// Err returns the wait result of the game process.
func (g *gameProcess) Err() error {
	<-g.exited

	return g.err
}

// reap waits for the process exactly once and publishes the result.
func (g *gameProcess) reap(waitable waiter) {
	g.err = waitable.Wait()

	close(g.exited)
}

// clearCloexec clears the close-on-exec flag of the file, so the
// descriptor survives the execve(2) of an agent upgrade and the next
// agent process can keep reading the game output.
func clearCloexec(file *os.File) error {
	descriptor := int(file.Fd())

	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), syscall.F_GETFD, 0)
	if errno != 0 {
		return fmt.Errorf("could not read the descriptor flags of %s: %w", file.Name(), errno)
	}

	_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), syscall.F_SETFD, flags&^syscall.FD_CLOEXEC)
	if errno != 0 {
		return fmt.Errorf("could not clear the close-on-exec flag of %s: %w", file.Name(), errno)
	}

	return nil
}

// parsePidEnv reads the pid and output file descriptors the previous
// agent process passed for the running game. A missing or incomplete
// set means the fresh start path.
func parsePidEnv(getenv func(string) string) (int, uintptr, uintptr, bool) {
	pid, err := strconv.Atoi(getenv(envFactorioPid))
	if err != nil || pid <= 0 {
		return 0, 0, 0, false
	}

	stdout, err := parseFdEnv(getenv(envFactorioStdoutFd))
	if err != nil {
		return 0, 0, 0, false
	}

	stderr, err := parseFdEnv(getenv(envFactorioStderrFd))
	if err != nil {
		return 0, 0, 0, false
	}

	return pid, stdout, stderr, true
}

// parseFdEnv reads one inherited file descriptor.
func parseFdEnv(value string) (uintptr, error) {
	descriptor, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("could not parse the descriptor %q: %w", value, err)
	}

	if descriptor < firstInheritedFd {
		return 0, fmt.Errorf("the descriptor %d is not an inherited descriptor: %w", descriptor, errGameGone)
	}

	return uintptr(descriptor), nil
}
