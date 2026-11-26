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

// Package controller implements the reconciliation logic for the koujouo API.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	capnprpc "capnproto.org/go/capnp/v3/rpc"
	corev1 "k8s.io/api/core/v1"

	rpcapi "koujouo.anqou.net/project/rpc"
)

var errPodHasNoIP = errors.New("pod has no IP assigned yet")

const (
	// agentRPCPort is the TCP port the agent serves Cap'n Proto RPC on.
	agentRPCPort = 9550

	// agentDialTimeout bounds a single dial attempt.
	agentDialTimeout = 5 * time.Second
)

// AgentClient bundles the Agent capability of one agent pod with the
// function that releases it and closes the underlying connection.
type AgentClient struct {
	// Agent is the Cap'n Proto capability of the agent.
	Agent rpcapi.Agent

	// ReleaseFunc releases the capability and closes the connection.
	// It may be nil.
	ReleaseFunc func()
}

// Release releases the agent capability and closes the connection.
func (c *AgentClient) Release() {
	if c.ReleaseFunc != nil {
		c.ReleaseFunc()
	}
}

// AgentDialer connects to the Cap'n Proto RPC server of an agent pod.
type AgentDialer interface {
	// Dial connects to the agent of pod and returns its capability.
	Dial(ctx context.Context, pod *corev1.Pod) (*AgentClient, error)
}

// TCPAgentDialer dials the agent over plain TCP to the pod IP on the
// agent RPC port and bootstraps the Agent capability.
type TCPAgentDialer struct{}

// Dial connects to the agent of pod over plain TCP.
func (d *TCPAgentDialer) Dial(ctx context.Context, pod *corev1.Pod) (*AgentClient, error) {
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("could not dial agent of Pod %s: %w", pod.Name, errPodHasNoIP)
	}

	address := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(agentRPCPort))

	dialer := &net.Dialer{Timeout: agentDialTimeout} //nolint:exhaustruct_v5 // Zero values are the intended defaults.

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("could not dial agent at %s: %w", address, err)
	}

	rpcConn := capnprpc.NewConn(capnprpc.NewStreamTransport(conn), nil)
	agent := rpcapi.Agent(rpcConn.Bootstrap(ctx))

	return &AgentClient{
		Agent: agent,
		ReleaseFunc: func() {
			agent.Release()

			_ = rpcConn.Close()
		},
	}, nil
}
