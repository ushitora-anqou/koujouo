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

package controller_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"

	capnp "capnproto.org/go/capnp/v3"
	capnprpc "capnproto.org/go/capnp/v3/rpc"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"koujouo.anqou.net/project/internal/controller"
	"koujouo.anqou.net/project/internal/version"
	rpcapi "koujouo.anqou.net/project/rpc"
)

var (
	errNoFakeAgent = errors.New("no fake agent is configured")

	// errFakeChecksum is the answer of an upgrade whose sha256 does not
	// match, like the real agent returns without execve(2)ing.
	errFakeChecksum = errors.New("the received agent binary is invalid")
)

// recordedConfig captures what the controller sent to configure.
type recordedConfig struct {
	gameVersion           string
	fileNames             []string
	discordWebhookURL     string
	leaveTimeoutSeconds   int32
	backupEndpointURL     string
	backupBucket          string
	backupAccessKeyID     string
	backupSecretAccessKey string
	downloadUsername      string
	downloadToken         string
}

// recordedUpgrade captures one agent upgrade the fake received.
type recordedUpgrade struct {
	// binary is the concatenation of the received chunks.
	binary []byte

	// sizes lists the sizes of the received chunks in order.
	sizes []int

	// sha256 is the checksum the stream ended with.
	sha256 []byte
}

// fakeAgentServer implements rpcapi.Agent_Server in memory. Configure
// applies the requested version and reports the running phase, like the
// real agent does after drain, download and restart.
type fakeAgentServer struct {
	mutex       sync.Mutex
	phase       rpcapi.Phase
	gameVersion string
	players     uint16
	configured  []recordedConfig
	stopped     int

	// agentVersion is the version the status reports; an upgrade
	// switches it to the version of this build unless keepAgentVersion
	// is set.
	agentVersion     string
	upgrades         []recordedUpgrade
	dropFirstChunk   bool
	keepAgentVersion bool

	// onExec runs in place of the execve(2) that ends an upgrade; the RPC
	// dialer uses it to drop the connection the upgrade arrived on.
	onExec func()

	// onConfigure, when set, runs in place of the immediate completion
	// of a configure call. Like the real agent, Configure releases the
	// question first (call.Go()), so Status keeps answering while the
	// hook walks the agent through draining and saving.
	onConfigure func(f *fakeAgentServer, recorded *recordedConfig)
}

// newFakeAgentServer returns a fake agent in the pending phase that
// runs the version of this build.
func newFakeAgentServer() *fakeAgentServer {
	return &fakeAgentServer{
		phase:        rpcapi.Phase_pending,
		agentVersion: version.Agent,
	}
}

// Configure records the sent configuration and reports running.
func (f *fakeAgentServer) Configure(_ context.Context, call rpcapi.Agent_configure) error {
	recorded, err := readConfigureArgs(call)
	if err != nil {
		return err
	}

	f.mutex.Lock()
	f.configured = append(f.configured, *recorded)
	hook := f.onConfigure
	f.mutex.Unlock()

	// The real agent keeps answering Status during a long configure, so
	// the question must not hold the server's single slot here either.
	call.Go()

	if hook != nil {
		hook(f, recorded)
	}

	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.gameVersion = recorded.gameVersion
	f.phase = rpcapi.Phase_running

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the configure results: %w", err)
	}

	return nil
}

// readConfigureArgs reads the configuration of a configure call.
func readConfigureArgs(call rpcapi.Agent_configure) (*recordedConfig, error) {
	config, err := call.Args().Cfg()
	if err != nil {
		return nil, fmt.Errorf("could not read the configure arguments: %w", err)
	}

	recorded := &recordedConfig{}

	recorded.gameVersion, err = config.GameVersion()
	if err != nil {
		return nil, fmt.Errorf("could not read the game version: %w", err)
	}

	recorded.fileNames, err = configFileNames(config)
	if err != nil {
		return nil, err
	}

	if config.HasDiscordWebhookURL() {
		recorded.discordWebhookURL, err = config.DiscordWebhookURL()
		if err != nil {
			return nil, fmt.Errorf("could not read the Discord webhook URL: %w", err)
		}
	}

	recorded.leaveTimeoutSeconds = config.LeaveTimeoutSeconds()

	err = readBackupSettings(config, recorded)
	if err != nil {
		return nil, err
	}

	err = readDownloadCredentials(config, recorded)
	if err != nil {
		return nil, err
	}

	return recorded, nil
}

// readDownloadCredentials reads the download credentials into recorded.
func readDownloadCredentials(config rpcapi.Config, recorded *recordedConfig) error {
	if config.HasDownloadUsername() {
		var err error

		recorded.downloadUsername, err = config.DownloadUsername()
		if err != nil {
			return fmt.Errorf("could not read the download username: %w", err)
		}
	}

	if config.HasDownloadToken() {
		var err error

		recorded.downloadToken, err = config.DownloadToken()
		if err != nil {
			return fmt.Errorf("could not read the download token: %w", err)
		}
	}

	return nil
}

// configFileNames returns the names of the delivered files.
func configFileNames(config rpcapi.Config) ([]string, error) {
	files, err := config.Files()
	if err != nil {
		return nil, fmt.Errorf("could not read the files: %w", err)
	}

	names := make([]string, 0, files.Len())

	for i := range files.Len() {
		name, err := files.At(i).Name()
		if err != nil {
			return nil, fmt.Errorf("could not read a file name: %w", err)
		}

		names = append(names, name)
	}

	return names, nil
}

// readBackupSettings reads the backup settings into recorded.
func readBackupSettings(config rpcapi.Config, recorded *recordedConfig) error {
	if !config.HasBackup() {
		return nil
	}

	backup, err := config.Backup()
	if err != nil {
		return fmt.Errorf("could not read the backup settings: %w", err)
	}

	recorded.backupEndpointURL, err = backup.EndpointURL()
	if err != nil {
		return fmt.Errorf("could not read the backup endpoint URL: %w", err)
	}

	recorded.backupBucket, err = backup.Bucket()
	if err != nil {
		return fmt.Errorf("could not read the backup bucket: %w", err)
	}

	recorded.backupAccessKeyID, err = backup.AccessKeyID()
	if err != nil {
		return fmt.Errorf("could not read the backup access key id: %w", err)
	}

	recorded.backupSecretAccessKey, err = backup.SecretAccessKey()
	if err != nil {
		return fmt.Errorf("could not read the backup secret access key: %w", err)
	}

	return nil
}

// Status reports the recorded agent state.
func (f *fakeAgentServer) Status(_ context.Context, call rpcapi.Agent_status) error {
	f.mutex.Lock()
	phase, gameVersion, players, agentVersion := f.phase, f.gameVersion, f.players, f.agentVersion
	f.mutex.Unlock()

	results, err := call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the status results: %w", err)
	}

	status, err := results.NewSt()
	if err != nil {
		return fmt.Errorf("could not allocate the AgentStatus struct: %w", err)
	}

	status.SetPhase(phase)

	err = status.SetGameVersion(gameVersion)
	if err != nil {
		return fmt.Errorf("could not set the game version: %w", err)
	}

	status.SetPlayers(players)

	err = status.SetAgentVersion(agentVersion)
	if err != nil {
		return fmt.Errorf("could not set the agent version: %w", err)
	}

	return nil
}

// Stop records the stop and reports the suspended phase.
func (f *fakeAgentServer) Stop(_ context.Context, call rpcapi.Agent_stop) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.stopped++
	f.phase = rpcapi.Phase_suspended

	_, err := call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the stop results: %w", err)
	}

	return nil
}

// Upgrade returns the ByteStream capability the controller pushes the
// new agent binary through.
func (f *fakeAgentServer) Upgrade(_ context.Context, call rpcapi.Agent_upgrade) error {
	results, err := call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the upgrade results: %w", err)
	}

	stream := rpcapi.ByteStream_ServerToClient(&fakeByteStream{agent: f})

	err = results.SetBin(stream)
	if err != nil {
		return fmt.Errorf("could not set the upgrade stream: %w", err)
	}

	return nil
}

// setOnConfigure installs the hook that a configure call runs through.
func (f *fakeAgentServer) setOnConfigure(hook func(f *fakeAgentServer, recorded *recordedConfig)) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.onConfigure = hook
}

// setPhase records the phase that Status reports next.
func (f *fakeAgentServer) setPhase(phase rpcapi.Phase) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.phase = phase
}

// fakeByteStream implements rpcapi.ByteStream_Server: it collects the
// chunks of one agent upgrade and verifies their sha256 when the stream
// ends, like the real agent before it execve(2)s the binary.
type fakeByteStream struct {
	agent  *fakeAgentServer
	buffer bytes.Buffer
	sizes  []int
}

// Write stores one chunk of the agent binary.
func (s *fakeByteStream) Write(_ context.Context, call rpcapi.ByteStream_write) error {
	chunk, err := call.Args().Chunk()
	if err != nil {
		return fmt.Errorf("could not read the chunk of the agent binary: %w", err)
	}

	s.agent.mutex.Lock()

	if s.agent.dropFirstChunk && len(s.sizes) == 0 {
		chunk = nil
	}

	_, err = s.buffer.Write(chunk)
	s.sizes = append(s.sizes, len(chunk))

	s.agent.mutex.Unlock()

	if err != nil {
		return fmt.Errorf("could not store the chunk of the agent binary: %w", err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the write results: %w", err)
	}

	return nil
}

// Done records the upgrade, verifies the sha256 and replaces the agent.
func (s *fakeByteStream) Done(_ context.Context, call rpcapi.ByteStream_done) error {
	sum, err := call.Args().Sha256()
	if err != nil {
		return fmt.Errorf("could not read the sha256 of the agent binary: %w", err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the done results: %w", err)
	}

	return s.agent.exec(s, sum)
}

// exec ends an upgrade the way the real agent does: it records what
// arrived, refuses a binary whose sha256 does not match, switches to the
// version of the new binary and runs in place of the execve(2).
func (f *fakeAgentServer) exec(stream *fakeByteStream, sum []byte) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.upgrades = append(f.upgrades, recordedUpgrade{
		binary: slices.Clone(stream.buffer.Bytes()),
		sizes:  slices.Clone(stream.sizes),
		sha256: slices.Clone(sum),
	})

	received := sha256.Sum256(stream.buffer.Bytes())

	if !bytes.Equal(received[:], sum) {
		return errFakeChecksum
	}

	if !f.keepAgentVersion {
		f.agentVersion = version.Agent
	}

	if f.onExec != nil {
		f.onExec()
	}

	return nil
}

// agentUpgrades returns the upgrades the fake received.
func (f *fakeAgentServer) agentUpgrades() []recordedUpgrade {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return slices.Clone(f.upgrades)
}

// currentAgentVersion returns the version the fake reports.
func (f *fakeAgentServer) currentAgentVersion() string {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return f.agentVersion
}

// setOnExec installs the hook that runs in place of the execve(2) ending
// an upgrade.
func (f *fakeAgentServer) setOnExec(onExec func()) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.onExec = onExec
}

// configurations returns the configurations the controller sent.
func (f *fakeAgentServer) configurations() []recordedConfig {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return append([]recordedConfig{}, f.configured...)
}

// stopCount returns how often stop was called.
func (f *fakeAgentServer) stopCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return f.stopped
}

// setGameVersion overrides the reported game version.
func (f *fakeAgentServer) setGameVersion(version string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.gameVersion = version
}

// setPlayers overrides the reported player count.
func (f *fakeAgentServer) setPlayers(players uint16) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.players = players
}

// setAgentVersion overrides the reported agent version.
func (f *fakeAgentServer) setAgentVersion(agentVersion string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.agentVersion = agentVersion
}

// fakeAgentDialer connects the controller to an in-process fake agent,
// or fails when no agent is configured. By default it hands out a direct
// capability; newRPCFakeAgentDialer switches it to a real Cap'n Proto
// connection over an in-process pipe, so an upgrade can drop the
// connection the way the execve(2) of the real agent does.
type fakeAgentDialer struct {
	agent *fakeAgentServer
	rpc   bool

	mutex   sync.Mutex
	dials   int
	servers []io.Closer
}

// newRPCFakeAgentDialer returns a dialer that connects to agent over a
// real Cap'n Proto connection. close must be called when the tests are
// done.
func newRPCFakeAgentDialer(agent *fakeAgentServer) *fakeAgentDialer {
	return &fakeAgentDialer{agent: agent, rpc: true}
}

// Dial connects the controller to the fake agent.
func (d *fakeAgentDialer) Dial(ctx context.Context, _ *corev1.Pod) (*controller.AgentClient, error) {
	d.mutex.Lock()
	d.dials++
	d.mutex.Unlock()

	if d.agent == nil {
		return nil, fmt.Errorf("could not dial the agent: %w", errNoFakeAgent)
	}

	if !d.rpc {
		capability := rpcapi.Agent_ServerToClient(d.agent)

		return &controller.AgentClient{
			Agent:       capability,
			ReleaseFunc: capability.Release,
		}, nil
	}

	serverEnd, clientEnd := net.Pipe()

	server := capnprpc.NewConn(
		capnprpc.NewStreamTransport(serverEnd),
		&capnprpc.Options{
			BootstrapClient: capnp.Client(rpcapi.Agent_ServerToClient(d.agent)),
		},
	)

	// The execve(2) of an upgrade closes the sockets of the agent process.
	d.agent.setOnExec(func() { _ = serverEnd.Close() })

	d.mutex.Lock()
	d.servers = append(d.servers, server)
	d.mutex.Unlock()

	client := capnprpc.NewConn(capnprpc.NewStreamTransport(clientEnd), nil)

	return &controller.AgentClient{
		Agent:       rpcapi.Agent(client.Bootstrap(ctx)),
		ReleaseFunc: func() { _ = client.Close() },
	}, nil
}

// dialCount returns how often the controller dialed the fake agent.
func (d *fakeAgentDialer) dialCount() int {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	return d.dials
}

// close closes the server ends of the connections the dialer made.
func (d *fakeAgentDialer) close() {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	for _, server := range d.servers {
		_ = server.Close()
	}
}

// fakeEventRecorder implements events.EventRecorder in memory.
type fakeEventRecorder struct {
	reasons []string
}

// newFakeEventRecorder returns an empty fake recorder.
func newFakeEventRecorder() *fakeEventRecorder {
	return &fakeEventRecorder{reasons: nil}
}

// Eventf records the reason of the event.
func (r *fakeEventRecorder) Eventf(
	_ k8sruntime.Object,
	_ k8sruntime.Object,
	_, reason, _, _ string,
	_ ...any,
) {
	r.reasons = append(r.reasons, reason)
}
