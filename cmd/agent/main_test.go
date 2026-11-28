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

package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	capnprpc "capnproto.org/go/capnp/v3/rpc"
	"github.com/go-logr/logr"

	"github.com/ushitora-anqou/koujouo/internal/agent"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// shutdownTestDeadline bounds the calls the tests wait for, so a shutdown
// or a signal wait that never returns fails fast instead of running into
// the timeout of go test.
const shutdownTestDeadline = 5 * time.Second

// errTestServe stands in for an RPC server that failed before the
// shutdown.
var errTestServe = errors.New("the RPC server failed")

// errTestGameCrash stands in for the error of a game that exited on its
// own.
var errTestGameCrash = errors.New("factorio exited unexpectedly")

// newTestAgent returns an agent on temporary volumes. It runs no game, so
// its Shutdown returns without waiting for a save.
func newTestAgent(t *testing.T) *agent.Agent {
	t.Helper()

	gameAgent, err := agent.New(logr.Discard(), agent.Options{
		SaveDir:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	return gameAgent
}

// newTestListener returns a loopback listener for the RPC server.
func newTestListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not open the test listener: %v", err)
	}

	return listener
}

// withinDeadline fails the test when call does not return within
// shutdownTestDeadline.
func withinDeadline(t *testing.T, what string, call func()) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		call()
	}()

	select {
	case <-done:
	case <-time.After(shutdownTestDeadline):
		t.Fatalf("%s did not return within %s", what, shutdownTestDeadline)
	}
}

// TestShutdownAbsorbsTheClosedListener verifies the SIGTERM path of the
// agent: closing the listener ends the RPC server with net.ErrClosed,
// which is the expected end of a shutdown and not a failure.
func TestShutdownAbsorbsTheClosedListener(t *testing.T) {
	t.Parallel()

	gameAgent := newTestAgent(t)

	listener := newTestListener(t)

	serveError := make(chan error, 1)

	go func() {
		// Serve returns with net.ErrClosed once the listener is closed.
		serveError <- capnprpc.Serve(listener, capnp.Client(rpcapi.Agent_ServerToClient(gameAgent)))
	}()

	withinDeadline(t, "shutdown", func() {
		err := shutdown(t.Context(), logr.Discard(), gameAgent, listener, serveError, nil)
		if err != nil {
			t.Errorf("shutdown() error = %v, want nil for a closed listener", err)
		}
	})
}

// TestShutdownReturnsTheServeError verifies that an RPC server which
// failed before the shutdown still reaches the caller: the error of the
// serve error channel is joined into the shutdown result instead of being
// dropped.
func TestShutdownReturnsTheServeError(t *testing.T) {
	t.Parallel()

	gameAgent := newTestAgent(t)

	listener := newTestListener(t)

	// The server already returned, so its error sits in the channel and no
	// goroutine serves the listener anymore.
	serveError := make(chan error, 1)
	serveError <- errTestServe

	withinDeadline(t, "shutdown", func() {
		err := shutdown(t.Context(), logr.Discard(), gameAgent, listener, serveError, nil)
		if !errors.Is(err, errTestServe) {
			t.Errorf("shutdown() error = %v, want %v", err, errTestServe)
		}
	})
}

// TestWaitForSignalReturnsTheServerError verifies that a failing RPC
// server ends the wait with its error, so the agent exits non-zero.
func TestWaitForSignalReturnsTheServerError(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	serveError := make(chan error, 1)
	gameFatal := make(chan error, 1)

	serveError <- errTestServe

	err := waitForSignal(logr.Discard(), signals, serveError, gameFatal)
	if !errors.Is(err, errTestServe) {
		t.Errorf("waitForSignal() error = %v, want %v", err, errTestServe)
	}
}

// TestWaitForSignalReturnsTheGameFatalError verifies that a game which
// exited on its own ends the wait with the error the agent reported, so
// the agent exits non-zero and the kubelet restarts the Pod.
func TestWaitForSignalReturnsTheGameFatalError(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	serveError := make(chan error, 1)
	gameFatal := make(chan error, 1)

	gameFatal <- errTestGameCrash

	withinDeadline(t, "waitForSignal", func() {
		err := waitForSignal(logr.Discard(), signals, serveError, gameFatal)
		if !errors.Is(err, errTestGameCrash) {
			t.Errorf("waitForSignal() error = %v, want %v", err, errTestGameCrash)
		}
	})
}

// TestWaitForSignalReturnsOnASignal verifies that a termination signal
// ends the wait without an error, which starts the graceful stop.
func TestWaitForSignalReturnsOnASignal(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	serveError := make(chan error, 1)
	gameFatal := make(chan error, 1)

	signals <- syscall.SIGTERM

	err := waitForSignal(logr.Discard(), signals, serveError, gameFatal)
	if err != nil {
		t.Errorf("waitForSignal() error = %v, want nil after a signal", err)
	}
}
