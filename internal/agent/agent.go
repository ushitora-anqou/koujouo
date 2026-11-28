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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/ushitora-anqou/koujouo/internal/version"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// Agent defaults.
const (
	playersPollInterval = 10 * time.Second
	maxPlayers          = int(^uint16(0))
	rconPasswordBytes   = 16
	privateFileMode     = 0o600
	directoryFileMode   = 0o750
	downloadTimeout     = 30 * time.Minute
)

// Agent errors.
var (
	// errDirsRequired is returned when the agent is created without its
	// directory options.
	errDirsRequired = errors.New("the save directory and the cache directory are required")

	// errTerminating is returned by a configure call that arrives after
	// the agent received its termination signal.
	errTerminating = errors.New("the agent is terminating")
)

// Options configures an Agent. The zero values of the optional fields
// fall back to the production defaults; tests override them to keep the
// loops fast and deterministic.
type Options struct {
	// SaveDir is the game write-data directory on the save volume.
	SaveDir string

	// CacheDir is the directory for the downloaded game binaries and
	// the received agent upgrades, on the ephemeral volume.
	CacheDir string

	// HTTPClient issues the game download and the webhook posts.
	// Defaults to a plain client.
	HTTPClient *http.Client

	// ExecBinary replaces this process; it exists so tests can observe
	// the upgrade exec. Defaults to syscall.Exec.
	ExecBinary func(path string, argv []string, env []string) error

	// DialRcon connects to the RCON endpoint of the game. Defaults to
	// the gorcon dialer.
	DialRcon rconDialer

	// PlayersPollInterval is how often the online players are polled.
	PlayersPollInterval time.Duration

	// DrainPollInterval is how often the players are polled during a
	// drain and how long to wait between the query retries.
	DrainPollInterval time.Duration

	// BackupPeriod is the period of the save uploads.
	BackupPeriod time.Duration

	// SaveWaitTimeout bounds how long the game may take to write the
	// save file after SIGINT.
	SaveWaitTimeout time.Duration

	// Now returns the current time; used for the backup object keys.
	Now func() time.Time
}

// Agent manages the Factorio process of one pod and implements the
// Agent capability the controller talks to.
type Agent struct {
	log        logr.Logger
	saveDir    string
	cacheDir   string
	httpClient *http.Client
	execBinary func(path string, argv []string, env []string) error
	dialRcon   rconDialer
	now        func() time.Time

	playersPollInterval time.Duration
	drainPollInterval   time.Duration
	backupPeriod        time.Duration
	saveWaitTimeout     time.Duration

	// mu serializes the lifecycle operations (configure, stop, upgrade
	// exec) and guards the fields below it. It is held for the whole
	// duration of an operation, which includes a drain.
	mu sync.Mutex

	phase        rpcapi.Phase
	config       *Config
	players      uint16
	game         gameHandle
	rconPort     int
	rconPassword string
	loopsCancel  context.CancelFunc
	terminating  bool

	// statusMu guards the status snapshot, so a status call never
	// waits for a lifecycle operation and can report a drain in
	// progress.
	statusMu sync.Mutex
	status   statusSnapshot

	// liveMu guards the settings that apply without a game restart,
	// which the output readers and the backup loop read concurrently
	// with a lifecycle operation.
	liveMu   sync.Mutex
	notifier *notifier
	backup   *uploader
}

// statusSnapshot is the state a status call reports.
type statusSnapshot struct {
	phase       rpcapi.Phase
	gameVersion string
	players     uint16
}

// New creates an Agent and prepares its directories.
func New(log logr.Logger, options Options) (*Agent, error) {
	if options.SaveDir == "" || options.CacheDir == "" {
		return nil, errDirsRequired
	}

	if log.GetSink() == nil {
		log = logr.Discard()
	}

	options.withDefaults()

	err := os.MkdirAll(filepath.Join(options.SaveDir, "saves"), directoryFileMode)
	if err != nil {
		return nil, fmt.Errorf("could not create the saves directory: %w", err)
	}

	err = os.MkdirAll(options.CacheDir, directoryFileMode)
	if err != nil {
		return nil, fmt.Errorf("could not create the cache directory: %w", err)
	}

	return &Agent{ //nolint:exhaustruct_v5 // Zero-valued state fields are the intended defaults.
		log:                 log,
		saveDir:             options.SaveDir,
		cacheDir:            options.CacheDir,
		httpClient:          options.HTTPClient,
		execBinary:          options.ExecBinary,
		dialRcon:            options.DialRcon,
		now:                 options.Now,
		playersPollInterval: options.PlayersPollInterval,
		drainPollInterval:   options.DrainPollInterval,
		backupPeriod:        options.BackupPeriod,
		saveWaitTimeout:     options.SaveWaitTimeout,
		status: statusSnapshot{ //nolint:exhaustruct_v5 // Reports no game before the first configure.
			phase: rpcapi.Phase_pending,
		},
	}, nil
}

// withDefaults fills the unset optional fields of the options.
func (o *Options) withDefaults() {
	if o.HTTPClient == nil {
		client := http.Client{Timeout: downloadTimeout} //nolint:exhaustruct_v5 // Only the overall timeout matters.
		o.HTTPClient = &client
	}

	if o.ExecBinary == nil {
		o.ExecBinary = execSyscall
	}

	if o.DialRcon == nil {
		o.DialRcon = dialRcon
	}

	if o.PlayersPollInterval <= 0 {
		o.PlayersPollInterval = playersPollInterval
	}

	if o.DrainPollInterval <= 0 {
		o.DrainPollInterval = drainPollInterval
	}

	if o.BackupPeriod <= 0 {
		o.BackupPeriod = defaultBackupPeriod
	}

	if o.SaveWaitTimeout <= 0 {
		o.SaveWaitTimeout = defaultSaveWaitTimeout
	}

	if o.Now == nil {
		o.Now = time.Now
	}
}

// Resume restores the game after a process start: an agent that was
// execve(2)d into during an upgrade re-attaches to the running game,
// otherwise the cached configuration resumes the game in the
// background while the RPC server already answers.
func (a *Agent) Resume(ctx context.Context) {
	pid, stdoutFd, stderrFd, ok := parsePidEnv(os.Getenv)
	if ok {
		err := a.resumeUpgraded(ctx, pid, stdoutFd, stderrFd)
		if err == nil {
			return
		}

		a.log.Error(err, "Could not resume from an upgrade, falling back to the cached configuration")
	}

	a.resumeFromState(ctx)
}

// Configure applies the full desired state. It is idempotent: an
// unchanged configuration of a running game returns immediately, a
// change of the settings that need no restart applies at once, and
// every other change drains and saves the running game first, then
// downloads the binary of the new version when needed and starts the
// game before returning.
func (a *Agent) Configure(ctx context.Context, call rpcapi.Agent_configure) error {
	// A Cap'n Proto server services one call at a time until the
	// handler calls Go. Applying the configuration drains, downloads
	// and starts, so the status calls must be serviced meanwhile.
	call.Go()

	config, err := readRPCConfig(call.Args())
	if err != nil {
		return fmt.Errorf("could not read the configure arguments: %w", err)
	}

	err = a.configure(ctx, config)
	if err != nil {
		return fmt.Errorf("could not apply the configuration: %w", err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the configure results: %w", err)
	}

	return nil
}

// Status reports the current agent and game state. It never waits for
// a lifecycle operation, so the controller can watch a drain progress.
func (a *Agent) Status(_ context.Context, call rpcapi.Agent_status) error {
	snapshot := a.snapshot()

	results, err := call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the status results: %w", err)
	}

	status, err := results.NewSt()
	if err != nil {
		return fmt.Errorf("could not allocate the AgentStatus struct: %w", err)
	}

	status.SetPhase(snapshot.phase)

	err = status.SetGameVersion(snapshot.gameVersion)
	if err != nil {
		return fmt.Errorf("could not set the game version: %w", err)
	}

	status.SetPlayers(snapshot.players)

	err = status.SetAgentVersion(version.Agent)
	if err != nil {
		return fmt.Errorf("could not set the agent version: %w", err)
	}

	return nil
}

// Stop gracefully stops the game and returns once the save file has
// been written and the game process has exited.
func (a *Agent) Stop(ctx context.Context, call rpcapi.Agent_stop) error {
	// Let the server service the status calls while the drain and the
	// save are in progress; see Configure.
	call.Go()

	err := a.stop(ctx)
	if err != nil {
		return fmt.Errorf("could not stop the game: %w", err)
	}

	_, err = call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the stop results: %w", err)
	}

	return nil
}

// Shutdown gracefully stops the game for good: it backs the SIGTERM
// handler, and a configure call that arrives afterwards does not start
// the game again.
func (a *Agent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.terminating = true

	return a.stopLocked(ctx)
}

// Upgrade returns the ByteStream capability the controller pushes the
// new agent binary through.
func (a *Agent) Upgrade(_ context.Context, call rpcapi.Agent_upgrade) error {
	receiver, err := newBinReceiver(a)
	if err != nil {
		return fmt.Errorf("could not receive the agent binary: %w", err)
	}

	results, err := call.AllocResults()
	if err != nil {
		return fmt.Errorf("could not allocate the upgrade results: %w", err)
	}

	stream := rpcapi.ByteStream_ServerToClient(receiver)

	err = results.SetBin(stream)
	if err != nil {
		return fmt.Errorf("could not set the upgrade stream: %w", err)
	}

	return nil
}

// resumeUpgraded adopts the game of the previous agent process.
func (a *Agent) resumeUpgraded(ctx context.Context, pid int, stdoutFd, stderrFd uintptr) error {
	game, err := reattachGameProcess(pid, stdoutFd, stderrFd)
	if err != nil {
		return fmt.Errorf("could not adopt the game of the previous agent: %w", err)
	}

	state, err := loadState(a.saveDir)
	if err != nil {
		return fmt.Errorf("could not load the state left by the previous agent: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.game = game
	a.players = 0

	if state != nil && state.Config != nil {
		a.applyRuntimeSettingsLocked(ctx, state)
	}

	a.startOutputReadersLocked(ctx, game)
	a.watchGame(game)
	a.startLoopsLocked(ctx)
	a.setPhaseLocked(rpcapi.Phase_running)

	a.log.Info("Resumed the game after an agent upgrade", "pid", pid, "gameVersion", a.gameVersionLocked())

	return nil
}

// resumeFromState restarts the game from the cached configuration in
// the background.
func (a *Agent) resumeFromState(ctx context.Context) {
	state, err := loadState(a.saveDir)
	if err != nil {
		a.log.Error(err, "Could not load the cached state")

		a.mu.Lock()
		a.setPhaseLocked(rpcapi.Phase_failed)
		a.mu.Unlock()

		return
	}

	if state == nil || state.Config == nil {
		a.log.Info("No cached configuration found, waiting for the controller")

		return
	}

	go a.resumeConfiguration(context.WithoutCancel(ctx), state.Config)
}

// resumeConfiguration applies a cached configuration under the
// lifecycle lock.
func (a *Agent) resumeConfiguration(ctx context.Context, config *Config) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.terminating {
		return
	}

	a.log.Info("Resuming the game from the cached configuration", "gameVersion", config.GameVersion)

	err := a.applyConfigLocked(ctx, config)
	if err != nil {
		a.log.Error(err, "Could not resume the game from the cached configuration")
	}
}

// applyRuntimeSettingsLocked adopts the configuration and the RCON
// parameters of a persisted state without touching the game.
func (a *Agent) applyRuntimeSettingsLocked(ctx context.Context, state *persistedState) {
	a.config = state.Config

	if state.RconPort != 0 {
		a.rconPort = state.RconPort
	}

	a.rconPassword = state.RconPassword
	a.applyLiveSettingsLocked(ctx, state.Config)
}

// applyLiveSettingsLocked switches to the settings of config that need
// no game restart. It must be called with the lifecycle lock held.
func (a *Agent) applyLiveSettingsLocked(ctx context.Context, config *Config) {
	a.setNotifier(newNotifier(config.DiscordWebhookURL, a.httpClient))
	a.setUploader(a.uploaderFor(ctx, config.Backup))
}

// uploaderFor builds the S3 uploader of the backup settings, or nil
// when the settings are absent or incomplete.
func (a *Agent) uploaderFor(ctx context.Context, settings *BackupSettings) *uploader {
	if settings == nil || settings.EndpointURL == "" || settings.Bucket == "" {
		return nil
	}

	upload, err := newUploader(ctx, settings)
	if err != nil {
		a.log.Error(err, "Could not build the S3 client", "bucket", settings.Bucket)

		return nil
	}

	return upload
}

// configure implements the configure logic with the lifecycle lock
// held.
func (a *Agent) configure(ctx context.Context, config *Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.game != nil && a.config != nil {
		if configsEqual(a.config, config) {
			a.log.Info("Configuration unchanged, nothing to do")

			return nil
		}

		if !a.config.needsRestart(config) {
			return a.applyLiveConfigurationLocked(ctx, config)
		}
	}

	if a.game != nil {
		err := a.stopLocked(ctx)
		if err != nil {
			return fmt.Errorf("could not stop the running game before reconfiguring: %w", err)
		}
	}

	return a.applyConfigLocked(ctx, config)
}

// applyLiveConfigurationLocked applies a configuration that differs
// from the running one only in settings that need no restart. It must
// be called with the lifecycle lock held.
func (a *Agent) applyLiveConfigurationLocked(ctx context.Context, config *Config) error {
	a.config = config
	a.applyLiveSettingsLocked(ctx, config)

	err := a.persistStateLocked()
	if err != nil {
		a.log.Error(err, "Could not persist the agent state")
	}

	a.log.Info("Applied the configuration without restarting the game")

	return nil
}

// applyConfigLocked writes the delivered files, ensures the game
// binary, starts the game and restarts the background loops. It must be
// called with the lifecycle lock held.
func (a *Agent) applyConfigLocked(ctx context.Context, config *Config) error {
	if a.terminating {
		return fmt.Errorf("could not start the game: %w", errTerminating)
	}

	a.config = config
	a.applyLiveSettingsLocked(ctx, config)

	err := a.writeConfigFilesLocked(config)
	if err != nil {
		return a.failLocked(err)
	}

	binary, err := a.ensureBinaryLocked(ctx, config)
	if err != nil {
		return a.failLocked(err)
	}

	a.stopLoopsLocked()

	err = a.startGameLocked(ctx, binary, config)
	if err != nil {
		return a.failLocked(err)
	}

	a.startLoopsLocked(ctx)

	err = a.persistStateLocked()
	if err != nil {
		a.log.Error(err, "Could not persist the agent state")
	}

	a.setPhaseLocked(rpcapi.Phase_running)

	return nil
}

// ensureBinaryLocked returns the path of the game binary of the
// configured version, downloading it when it is not cached yet. It must
// be called with the lifecycle lock held.
func (a *Agent) ensureBinaryLocked(ctx context.Context, config *Config) (string, error) {
	binary := gameBinaryPath(a.cacheDir, config.GameVersion)

	_, statErr := os.Stat(binary)
	if statErr == nil {
		return binary, nil
	}

	a.setPhaseLocked(rpcapi.Phase_downloading)

	binary, err := ensureBinary(ctx, a.log, a.httpClient,
		a.cacheDir, config.GameVersion, config.DownloadUsername, config.DownloadToken)
	if err != nil {
		return "", fmt.Errorf("could not ensure the game binary: %w", err)
	}

	return binary, nil
}

// failLocked records a failed application of a configuration. It must be
// called with the lifecycle lock held.
func (a *Agent) failLocked(err error) error {
	a.setPhaseLocked(rpcapi.Phase_failed)
	a.log.Error(err, "Could not apply the configuration")

	return err
}

// writeConfigFilesLocked writes the delivered files into the save
// directory. The config.ini write-data path is rewritten to the save
// directory; unknown file names are ignored. A configuration that
// delivers no config.ini gets a minimal one, because the game is always
// started with -c <saveDir>/config.ini: without it the game writes its
// data outside the save volume and loses the autosaves. It must be
// called with the lifecycle lock held.
func (a *Agent) writeConfigFilesLocked(config *Config) error {
	deliversConfigIni := slices.ContainsFunc(config.Files, func(file ConfigFile) bool {
		return file.Name == configFileIniName
	})

	if !deliversConfigIni {
		err := a.writeConfigFileLocked(ConfigFile{Name: configFileIniName, Content: writeDataSectionHeader + "\n"})
		if err != nil {
			return fmt.Errorf("could not write the default config.ini: %w", err)
		}
	}

	for _, file := range config.Files {
		err := a.writeConfigFileLocked(file)
		if err != nil {
			return fmt.Errorf("could not write the delivered file %s: %w", file.Name, err)
		}
	}

	return nil
}

// writeConfigFileLocked writes one delivered file into the save
// directory.
func (a *Agent) writeConfigFileLocked(file ConfigFile) error {
	switch file.Name {
	case configFileIniName:
		content := rewriteWriteData(file.Content, a.saveDir)

		return writeFileSync(filepath.Join(a.saveDir, configFileIniName), []byte(content), privateFileMode)
	case serverWhitelistName:
		return writeFileSync(filepath.Join(a.saveDir, serverWhitelistName), []byte(file.Content), privateFileMode)
	default:
		a.log.Info("Ignored an unknown delivered file", "name", file.Name)

		return nil
	}
}

// startGameLocked starts the game process, creating the save file when
// it does not exist yet. It must be called with the lifecycle lock held.
func (a *Agent) startGameLocked(ctx context.Context, binary string, config *Config) error {
	savePath := a.savePath()

	err := a.ensureSaveLocked(ctx, binary, config, savePath)
	if err != nil {
		return err
	}

	err = a.ensureRconSettingsLocked()
	if err != nil {
		return err
	}

	game, err := startGameProcess(a.saveDir, startParams{
		binary:        binary,
		configPath:    filepath.Join(a.saveDir, configFileIniName),
		savePath:      savePath,
		whitelistPath: a.whitelistPathLocked(config),
		rconPort:      a.rconPort,
		rconPassword:  a.rconPassword,
		create:        false,
	})
	if err != nil {
		return fmt.Errorf("could not start the game: %w", err)
	}

	a.game = game
	a.setPlayersLocked(0)

	a.startOutputReadersLocked(ctx, game)
	a.watchGame(game)

	a.log.Info("Started Factorio", "gameVersion", config.GameVersion, "pid", game.Pid())

	return nil
}

// ensureSaveLocked creates the save file when it does not exist yet. It
// must be called with the lifecycle lock held.
func (a *Agent) ensureSaveLocked(ctx context.Context, binary string, config *Config, savePath string) error {
	_, statErr := os.Stat(savePath)
	if statErr == nil {
		return nil
	}

	if !os.IsNotExist(statErr) {
		return fmt.Errorf("could not stat the save file %s: %w", savePath, statErr)
	}

	return a.createSaveLocked(ctx, binary, config, savePath)
}

// ensureRconSettingsLocked fills the RCON port and password once. It
// must be called with the lifecycle lock held.
func (a *Agent) ensureRconSettingsLocked() error {
	if a.rconPort == 0 {
		a.rconPort = rconDefaultPort
	}

	if a.rconPassword != "" {
		return nil
	}

	password, err := randomPassword()
	if err != nil {
		return fmt.Errorf("could not generate the RCON password: %w", err)
	}

	a.rconPassword = password

	return nil
}

// createSaveLocked runs the game once with --create to write a fresh
// save file. It must be called with the lifecycle lock held.
func (a *Agent) createSaveLocked(ctx context.Context, binary string, config *Config, savePath string) error {
	a.log.Info("No save found, creating one", "path", savePath)

	params := startParams{
		binary:        binary,
		configPath:    filepath.Join(a.saveDir, configFileIniName),
		savePath:      savePath,
		whitelistPath: a.whitelistPathLocked(config),
		rconPort:      0,
		rconPassword:  "",
		create:        true,
	}

	args := factorioArgs(params)

	// The create run is bounded by the call that asked for it.
	command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // Agent-built path.
	command.Dir = a.saveDir

	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not create the save %s: %w: %s", savePath, err, output)
	}

	// The create run has no output readers, so its output is logged here.
	a.log.Info("Created the save file", "path", savePath, "output", strings.TrimSpace(string(output)))

	return nil
}

// whitelistPathLocked returns the path of the server whitelist file, or
// an empty string when the configuration delivers none. It must be
// called with the lifecycle lock held.
func (a *Agent) whitelistPathLocked(config *Config) string {
	if !config.hasServerWhitelist() {
		return ""
	}

	return filepath.Join(a.saveDir, serverWhitelistName)
}

// watchGame marks the agent failed when the game exits on its own.
func (a *Agent) watchGame(game gameHandle) {
	go func() {
		<-game.Exited()

		err := game.Err()

		a.mu.Lock()
		defer a.mu.Unlock()

		// A game that was replaced or stopped in the meantime is not
		// the one the agent runs anymore.
		if a.game != game {
			return
		}

		a.stopLoopsLocked()

		a.game = nil
		a.setPlayersLocked(0)

		a.log.Error(err, "Factorio exited unexpectedly")

		a.setPhaseLocked(rpcapi.Phase_failed)
	}()
}

// startOutputReadersLocked mirrors the game output to the agent log and
// forwards the player events to the notifier. The readers follow the
// game process, not the lifetime of the call that started them. It must
// be called with the lifecycle lock held.
func (a *Agent) startOutputReadersLocked(ctx context.Context, game gameHandle) {
	ctx = context.WithoutCancel(ctx)

	go copyGameOutput(ctx, a.log, game.Stdout(), streamStdout, a.currentNotifier)
	go copyGameOutput(ctx, a.log, game.Stderr(), streamStderr, a.currentNotifier)
}

// startLoopsLocked starts the players poll and the backup loop of the
// running game. The loops follow the game process, not the lifetime of
// the call that started them. It must be called with the lifecycle lock
// held.
func (a *Agent) startLoopsLocked(ctx context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.loopsCancel = cancel

	go a.playersPollLoop(ctx)
	go a.backupLoop(ctx)
}

// stopLoopsLocked stops the players poll and the backup loop. It must be
// called with the lifecycle lock held.
func (a *Agent) stopLoopsLocked() {
	if a.loopsCancel != nil {
		a.loopsCancel()
		a.loopsCancel = nil
	}
}

// playersPollLoop polls the online players while the game runs.
func (a *Agent) playersPollLoop(ctx context.Context) {
	ticker := time.NewTicker(a.playersPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.updatePlayers(ctx)
		}
	}
}

// updatePlayers stores the last successful player count, following
// design.md section 8.4: a failed poll keeps the previous value.
func (a *Agent) updatePlayers(ctx context.Context) {
	a.mu.Lock()
	game, port, password := a.game, a.rconPort, a.rconPassword
	a.mu.Unlock()

	if game == nil || port == 0 || password == "" {
		return
	}

	conn, err := a.dialRcon(rconAddress(port), password)
	if err != nil {
		return // Keep the last value.
	}

	defer conn.Close() //nolint:errcheck // The poll connection needs no drain.

	pollCtx, cancel := context.WithTimeout(ctx, rconDeadline)
	defer cancel()

	players, err := conn.PlayersOnline(pollCtx)
	if err != nil {
		return // Keep the last value.
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// The game the count belongs to may have been stopped meanwhile.
	if a.game != game || ctx.Err() != nil {
		return
	}

	a.setPlayersLocked(clampPlayers(players))
}

// setPlayersLocked stores the online player count and publishes it. It
// must be called with the lifecycle lock held.
func (a *Agent) setPlayersLocked(players uint16) {
	a.players = players
	a.publishLocked()
}

// setNotifier switches the notifier the output readers use.
func (a *Agent) setNotifier(notify *notifier) {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()

	a.notifier = notify
}

// currentNotifier returns the notifier to use for a new event.
func (a *Agent) currentNotifier() *notifier {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()

	return a.notifier
}

// setUploader switches the uploader the backups use.
func (a *Agent) setUploader(upload *uploader) {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()

	a.backup = upload
}

// currentUploader returns the uploader to use for a new backup, or nil
// when backups are disabled.
func (a *Agent) currentUploader() *uploader {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()

	return a.backup
}

// snapshot returns the state a status call reports.
func (a *Agent) snapshot() statusSnapshot {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	return a.status
}

// publishLocked copies the current state into the status snapshot. It
// must be called with the lifecycle lock held.
func (a *Agent) publishLocked() {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	a.status = statusSnapshot{
		phase:       a.phase,
		gameVersion: a.gameVersionLocked(),
		players:     a.players,
	}
}

// gameVersionLocked returns the game version of the last applied
// configuration, which is empty before the first configure. It must be
// called with the lifecycle lock held.
func (a *Agent) gameVersionLocked() string {
	if a.config == nil {
		return ""
	}

	return a.config.GameVersion
}

// stop stops the game under the lifecycle lock.
func (a *Agent) stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.stopLocked(ctx)
}

// readRPCConfig reads the Config struct of a configure call.
func readRPCConfig(params rpcapi.Agent_configure_Params) (*Config, error) {
	rpcConfig, err := params.Cfg()
	if err != nil {
		return nil, fmt.Errorf("could not read the Config struct: %w", err)
	}

	config := &Config{LeaveTimeoutSeconds: rpcConfig.LeaveTimeoutSeconds()} //nolint:exhaustruct_v5 // Filled in below.

	config.GameVersion, err = rpcConfig.GameVersion()
	if err != nil {
		return nil, fmt.Errorf("could not read the game version: %w", err)
	}

	err = readRPCFiles(rpcConfig, config)
	if err != nil {
		return nil, err
	}

	err = readRPCBackup(rpcConfig, config)
	if err != nil {
		return nil, err
	}

	err = readRPCCredentials(rpcConfig, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// readRPCCredentials reads the optional Discord webhook URL and the
// download credentials into config.
func readRPCCredentials(rpcConfig rpcapi.Config, config *Config) error {
	var err error

	if rpcConfig.HasDiscordWebhookURL() {
		config.DiscordWebhookURL, err = rpcConfig.DiscordWebhookURL()
		if err != nil {
			return fmt.Errorf("could not read the Discord webhook URL: %w", err)
		}
	}

	if rpcConfig.HasDownloadUsername() {
		config.DownloadUsername, err = rpcConfig.DownloadUsername()
		if err != nil {
			return fmt.Errorf("could not read the download username: %w", err)
		}
	}

	if rpcConfig.HasDownloadToken() {
		config.DownloadToken, err = rpcConfig.DownloadToken()
		if err != nil {
			return fmt.Errorf("could not read the download token: %w", err)
		}
	}

	return nil
}

// readRPCFiles reads the delivered files into config.
func readRPCFiles(rpcConfig rpcapi.Config, config *Config) error {
	files, err := rpcConfig.Files()
	if err != nil {
		return fmt.Errorf("could not read the files: %w", err)
	}

	config.Files = make([]ConfigFile, 0, files.Len())

	for index := range files.Len() {
		entry := files.At(index)

		name, err := entry.Name()
		if err != nil {
			return fmt.Errorf("could not read a file name: %w", err)
		}

		content, err := entry.Content()
		if err != nil {
			return fmt.Errorf("could not read the content of file %s: %w", name, err)
		}

		config.Files = append(config.Files, ConfigFile{Name: name, Content: string(content)})
	}

	return nil
}

// readRPCBackup reads the backup settings into config. Incomplete
// settings leave the backup disabled, following the schema comment that
// a zero value disables backups.
func readRPCBackup(rpcConfig rpcapi.Config, config *Config) error {
	if !rpcConfig.HasBackup() {
		return nil
	}

	backup, err := rpcConfig.Backup()
	if err != nil {
		return fmt.Errorf("could not read the backup settings: %w", err)
	}

	settings := &BackupSettings{} //nolint:exhaustruct_v5 // Filled in below.

	settings.EndpointURL, err = backup.EndpointURL()
	if err != nil {
		return fmt.Errorf("could not read the backup endpoint URL: %w", err)
	}

	settings.Bucket, err = backup.Bucket()
	if err != nil {
		return fmt.Errorf("could not read the backup bucket: %w", err)
	}

	settings.AccessKeyID, err = backup.AccessKeyID()
	if err != nil {
		return fmt.Errorf("could not read the backup access key id: %w", err)
	}

	settings.SecretAccessKey, err = backup.SecretAccessKey()
	if err != nil {
		return fmt.Errorf("could not read the backup secret access key: %w", err)
	}

	if settings.EndpointURL == "" || settings.Bucket == "" {
		return nil
	}

	config.Backup = settings

	return nil
}

// randomPassword returns a random hex string for the RCON password.
func randomPassword() (string, error) {
	buffer := make([]byte, rconPasswordBytes)

	_, err := rand.Read(buffer)
	if err != nil {
		return "", fmt.Errorf("could not read random bytes: %w", err)
	}

	return hex.EncodeToString(buffer), nil
}

// clampPlayers limits a player count to what the protocol can report.
func clampPlayers(players int) uint16 {
	if players > maxPlayers {
		return uint16(maxPlayers)
	}

	if players < 0 {
		return 0
	}

	return uint16(players)
}

// savePath returns the save file the game is started with.
func (a *Agent) savePath() string {
	return filepath.Join(a.savesDir(), saveFileName)
}

// savesDir returns the directory holding the save files.
func (a *Agent) savesDir() string {
	return filepath.Join(a.saveDir, "saves")
}

// persistStateLocked writes the state file. It must be called with the
// lifecycle lock held.
func (a *Agent) persistStateLocked() error {
	err := saveState(a.saveDir, &persistedState{
		Config:       a.config,
		RconPort:     a.rconPort,
		RconPassword: a.rconPassword,
	})
	if err != nil {
		return fmt.Errorf("could not persist the agent state: %w", err)
	}

	return nil
}

// setPhaseLocked records the phase and publishes the status. It must be
// called with the lifecycle lock held.
func (a *Agent) setPhaseLocked(phase rpcapi.Phase) {
	if a.phase != phase {
		a.log.Info("Agent phase changed", "phase", phase.String())
	}

	a.phase = phase
	a.publishLocked()
}

// needsRestart reports whether a game running with the configuration c
// must be restarted to run with other: a game version change or a
// change of the delivered files needs a new process, the other settings
// apply to the running game.
func (c *Config) needsRestart(other *Config) bool {
	return c.GameVersion != other.GameVersion || !slices.Equal(c.Files, other.Files)
}

// execSyscall executes the binary, replacing this process.
func execSyscall(path string, argv []string, env []string) error {
	//nolint:gosec // The path is the verified agent upgrade target.
	err := syscall.Exec(path, argv, env)
	if err != nil {
		return fmt.Errorf("could not exec %s: %w", path, err)
	}

	return nil
}
