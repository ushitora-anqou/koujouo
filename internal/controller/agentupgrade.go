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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"capnproto.org/go/capnp/v3/exc"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	koujouoanqounetv1 "koujouo.anqou.net/project/api/v1"
	"koujouo.anqou.net/project/internal/version"
	rpcapi "koujouo.anqou.net/project/rpc"
)

const (
	// defaultAgentBinaryPath is where the controller image holds the
	// agent binary of the same build as the manager.
	defaultAgentBinaryPath = "/kojouo/agent"

	// agentBinaryChunkSize is the size of the chunks the agent binary is
	// streamed in.
	agentBinaryChunkSize = 512 * 1024

	// upgradeRedialPause is the wait between two attempts to reach the
	// agent process an upgrade execve(2)d into.
	upgradeRedialPause = 200 * time.Millisecond
)

// errAgentNotUpgraded marks an agent that reports another version than
// this build after it received the agent binary.
var errAgentNotUpgraded = errors.New("the agent still reports another version after the upgrade")

// agentBinaryPath returns the path of the agent binary the outdated
// agents receive.
func (r *FactorioServerReconciler) agentBinaryPath() string {
	if r.AgentBinaryPath != "" {
		return r.AgentBinaryPath
	}

	return defaultAgentBinaryPath
}

// needsAgentUpgrade reports whether the agent of pod runs another build
// than the controller and must receive the agent binary of its image. An
// agent that reports no version predates the version field and is
// upgraded too. The agent of a Pod that is being deleted is left alone:
// its replacement starts with the current image anyway.
func needsAgentUpgrade(pod *corev1.Pod, reported *agentStatus) bool {
	return reported.agentVersion != version.Agent && pod.DeletionTimestamp == nil
}

// upgradeAgent pushes the agent binary of the controller image to the
// agent, which execve(2)s it and thereby drops the connection the binary
// arrived on. It returns the connection to the upgraded agent with its
// status. The caller keeps owning the capability of the outdated agent.
func (r *FactorioServerReconciler) upgradeAgent(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
	pod *corev1.Pod,
	agent rpcapi.Agent,
	outdated string,
) (*AgentClient, *agentStatus, error) {
	err := r.pushAgentBinary(ctx, agent)
	if err != nil {
		return nil, nil, err
	}

	upgraded, reported, err := r.dialUpgradedAgent(ctx, pod)
	if err != nil {
		return nil, nil, err
	}

	log.Info("Upgraded agent", "pod", pod.Name, "fromVersion", outdated, "toVersion", version.Agent)
	r.recordEvent(server, "AgentUpgraded",
		"Upgraded the agent of Pod "+pod.Name+" from version "+outdated+" to "+version.Agent)

	return upgraded, reported, nil
}

// pushAgentBinary streams the agent binary of the controller image to
// agent and ends the stream with its sha256. The agent execve(2)s the new
// binary to complete the upgrade, which closes the connection before it
// can answer, so a stream that ends in a disconnect is accepted too;
// dialUpgradedAgent then confirms that the new agent answers.
func (r *FactorioServerReconciler) pushAgentBinary(ctx context.Context, agent rpcapi.Agent) error {
	binary, err := os.Open(r.agentBinaryPath())
	if err != nil {
		return fmt.Errorf("could not open the agent binary: %w", err)
	}

	defer binary.Close() //nolint:errcheck // The binary was only read.

	future, release := agent.Upgrade(ctx, nil)
	defer release()

	results, err := future.Struct()
	if err != nil {
		return fmt.Errorf("agent upgrade call failed: %w", err)
	}

	stream := results.Bin()

	defer stream.Release()

	hash := sha256.New()

	err = streamAgentBinary(ctx, stream, io.TeeReader(binary, hash))
	if err != nil {
		return err
	}

	done, releaseDone := stream.Done(ctx, func(params rpcapi.ByteStream_done_Params) error {
		return params.SetSha256(hash.Sum(nil))
	})

	defer releaseDone()

	_, err = done.Struct()
	if err != nil && !exc.IsType(err, exc.Disconnected) {
		return fmt.Errorf("the agent did not accept the agent binary: %w", err)
	}

	return nil
}

// streamAgentBinary sends binary in chunks through stream and waits for
// the acknowledgment of each chunk before sending the next one, so the
// agent sets the pace of the transfer.
func streamAgentBinary(ctx context.Context, stream rpcapi.ByteStream, binary io.Reader) error {
	chunk := make([]byte, agentBinaryChunkSize)

	for {
		read, readErr := io.ReadFull(binary, chunk)

		if read > 0 {
			err := writeChunk(ctx, stream, chunk[:read])
			if err != nil {
				return err
			}
		}

		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil
		}

		if readErr != nil {
			return fmt.Errorf("could not read the agent binary: %w", readErr)
		}
	}
}

// writeChunk sends one chunk of the agent binary and waits until the
// agent stored it.
func writeChunk(ctx context.Context, stream rpcapi.ByteStream, chunk []byte) error {
	err := stream.Write(ctx, func(params rpcapi.ByteStream_write_Params) error {
		return params.SetChunk(chunk)
	})
	if err != nil {
		return fmt.Errorf("could not send the agent binary: %w", err)
	}

	err = waitForStream(ctx, stream)
	if err != nil {
		return fmt.Errorf("the agent did not store a chunk of the agent binary: %w", err)
	}

	return nil
}

// waitForStream waits for the streaming calls sent so far to complete
// and reports the first error among them. It gives up when ctx ends; the
// caller then closes the connection, which also ends the wait.
func waitForStream(ctx context.Context, stream rpcapi.ByteStream) error {
	completed := make(chan error, 1)

	go func() {
		completed <- stream.WaitStreaming()
	}()

	select {
	case err := <-completed:
		if err != nil {
			return fmt.Errorf("agent write call failed: %w", err)
		}

		return nil
	case <-ctx.Done():
		return fmt.Errorf("could not wait for the agent to store the agent binary: %w", ctx.Err())
	}
}

// dialUpgradedAgent connects to the agent of pod again and returns its
// connection once the agent reports the version of this build. The
// process an upgrade execve(2)d into needs a moment to listen again, so
// the attempts are repeated for the retry interval.
func (r *FactorioServerReconciler) dialUpgradedAgent(
	ctx context.Context,
	pod *corev1.Pod,
) (*AgentClient, *agentStatus, error) {
	deadline := time.Now().Add(r.retryInterval())

	for {
		upgraded, reported, err := r.dialAgentStatus(ctx, pod)

		switch {
		case err == nil:
			if reported.agentVersion != version.Agent {
				upgraded.Release()

				return nil, nil, fmt.Errorf("%w: got %q, want %q",
					errAgentNotUpgraded, reported.agentVersion, version.Agent)
			}

			return upgraded, reported, nil
		case time.Now().After(deadline):
			return nil, nil, fmt.Errorf("could not reach the upgraded agent: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("could not wait for the upgraded agent: %w", ctx.Err())
		case <-time.After(upgradeRedialPause):
		}
	}
}

// dialAgentStatus connects to the agent of pod and reads its status. A
// connection whose status cannot be read is released again.
func (r *FactorioServerReconciler) dialAgentStatus(
	ctx context.Context,
	pod *corev1.Pod,
) (*AgentClient, *agentStatus, error) {
	agent, err := r.Dialer.Dial(ctx, pod)
	if err != nil {
		return nil, nil, fmt.Errorf("could not dial the agent: %w", err)
	}

	reported, err := r.callAgentStatus(ctx, agent.Agent)
	if err != nil {
		agent.Release()

		return nil, nil, err
	}

	return agent, reported, nil
}
