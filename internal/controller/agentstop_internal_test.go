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

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

const (
	// stopTestDeadline is the reconcile deadline the stop test passes in.
	// It is short so a stop that honoured it would return at once.
	stopTestDeadline = 100 * time.Millisecond

	// stopTestWindow is how long the test watches the call after the
	// deadline passed.
	stopTestWindow = 4 * stopTestDeadline

	// stopTestWait bounds the wait for the call once the agent answered.
	stopTestWait = 5 * time.Second

	// stopTestToken is the RPC token the stop call carries.
	stopTestToken = "stop-test-token"
)

// errUnexpectedCall marks an agent method the stop test never expects.
var errUnexpectedCall = errors.New("unexpected agent call")

// hangingAgent answers no stop until the test releases it, like an agent
// that drains for its leave timeout before it saves the world.
type hangingAgent struct {
	release <-chan struct{}
}

// Configure is not part of the stop.
func (a *hangingAgent) Configure(_ context.Context, _ rpcapi.Agent_configure) error {
	return errUnexpectedCall
}

// Status is not part of the stop.
func (a *hangingAgent) Status(_ context.Context, _ rpcapi.Agent_status) error {
	return errUnexpectedCall
}

// Stop waits for the release, then reports the stop.
func (a *hangingAgent) Stop(_ context.Context, call rpcapi.Agent_stop) error {
	<-a.release

	_, err := call.AllocResults()

	return err
}

// Upgrade is not part of the stop.
func (a *hangingAgent) Upgrade(_ context.Context, _ rpcapi.Agent_upgrade) error {
	return errUnexpectedCall
}

// TestCallAgentStopIgnoresTheReconcileDeadline verifies the documented
// behaviour of a stop call: it runs under context.WithoutCancel, because a
// drain waits for the leave timeout the user may set past the reconcile
// deadline of controller-runtime. The deadline therefore does not end the
// call; cancelling the reconcile context does (only a controller shutdown
// does that). A stop that returned at the deadline would leave the game
// running while its StatefulSet is deleted.
func TestCallAgentStopIgnoresTheReconcileDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	capability := rpcapi.Agent_ServerToClient(&hangingAgent{release: release})

	t.Cleanup(func() { capability.Release() })

	reconciler := &FactorioServerReconciler{}

	ctx, cancel := context.WithTimeout(t.Context(), stopTestDeadline)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- reconciler.callAgentStop(ctx, capability, stopTestToken, nil)
	}()

	select {
	case err := <-done:
		t.Fatalf("callAgentStop() returned before the agent answered the stop (error %v)", err)
	case <-time.After(stopTestWindow):
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("callAgentStop() error = %v, want nil after the agent answered", err)
		}
	case <-time.After(stopTestWait):
		t.Fatal("callAgentStop() did not return once the agent answered the stop")
	}
}
