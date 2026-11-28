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

// Package main runs the koujouo agent, the PID 1 entrypoint of the
// Factorio StatefulSet pod. The agent serves the Cap'n Proto RPC
// protocol the controller talks to and manages the game process
// lifecycle, including the graceful stop on SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	capnprpc "capnproto.org/go/capnp/v3/rpc"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ushitora-anqou/koujouo/internal/agent"
	"github.com/ushitora-anqou/koujouo/internal/version"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// defaultConfigPath is where the agent reads its configuration when --config
// is not given. The image of the agent holds the file.
const defaultConfigPath = "/etc/koujouo/agent.yaml"

// Agent defaults. The shutdown deadline stays below the 900 seconds
// termination grace period of the StatefulSet, so the graceful stop
// finishes before the kubelet would kill the pod.
//
// examples/agent.yaml lists the same keys with the same values.
const (
	defaultListenAddress  = ":9550"
	defaultSaveDir        = "/var/lib/koujouo"
	defaultCacheDir       = "/opt/koujouo"
	defaultLogDevelopment = false
	defaultLogLevel       = "info"
	shutdownTimeout       = 9 * time.Minute
)

// logConfig is the log section of the configuration.
type logConfig struct {
	// Development selects the human-readable console encoding instead of
	// JSON.
	Development bool `mapstructure:"development"`

	// Level is the minimum level the agent logs: debug, info, warn, error,
	// dpanic, panic or fatal.
	Level string `mapstructure:"level"`
}

// agentConfig is the configuration of the agent, read from the YAML file
// given with --config.
type agentConfig struct {
	// ListenAddress is the address the Cap'n Proto RPC server listens on.
	// The manager connects to the port of the pod IP.
	ListenAddress string `mapstructure:"listenAddress"`

	// SaveDir is the game write-data directory on the save volume.
	SaveDir string `mapstructure:"saveDir"`

	// CacheDir is the game binary and upgrade cache directory on the
	// ephemeral volume.
	CacheDir string `mapstructure:"cacheDir"`

	Log logConfig `mapstructure:"log"`
}

func main() {
	err := newRootCommand().Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

// newRootCommand builds the root command of the agent, whose only flag names
// the configuration file.
func newRootCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{ //nolint:exhaustruct_v5 // The zero values of cobra.Command are valid.
		Use:   "agent",
		Short: "Run the koujouo agent",
		Long: "Run the koujouo agent, the PID 1 process of a Factorio StatefulSet pod.\n" +
			"It serves the Cap'n Proto RPC protocol of the controller and manages the\n" +
			"game process, including the graceful stop on SIGTERM.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}

			logger, err := newLogger(cfg.Log)
			if err != nil {
				return err
			}

			err = run(context.Background(), logger, cfg)
			if err != nil {
				return err
			}

			logger.Info("Stopped agent")

			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath,
		"path to the YAML configuration file")

	return cmd
}

// loadConfig reads the YAML configuration file at path over the built-in
// defaults, which fill the keys the file leaves out.
func loadConfig(path string) (agentConfig, error) {
	var cfg agentConfig

	settings := viper.New()

	settings.SetDefault("listenAddress", defaultListenAddress)
	settings.SetDefault("saveDir", defaultSaveDir)
	settings.SetDefault("cacheDir", defaultCacheDir)
	settings.SetDefault("log.development", defaultLogDevelopment)
	settings.SetDefault("log.level", defaultLogLevel)

	settings.SetConfigFile(path)
	settings.SetConfigType("yaml")

	err := settings.ReadInConfig()
	if err != nil {
		return cfg, fmt.Errorf("could not read the configuration file %s: %w", path, err)
	}

	err = settings.Unmarshal(&cfg)
	if err != nil {
		return cfg, fmt.Errorf("could not decode the configuration file %s: %w", path, err)
	}

	return cfg, nil
}

// newLogger builds the logger of the agent from the log section of the
// configuration.
func newLogger(cfg logConfig) (logr.Logger, error) {
	var (
		level  zapcore.Level
		logger logr.Logger
	)

	err := level.Set(cfg.Level)
	if err != nil {
		return logger, fmt.Errorf("could not parse the log level %q: %w", cfg.Level, err)
	}

	options := zap.Options{
		Development: cfg.Development,
		Level:       level,
	}

	return zap.New(zap.UseFlagOptions(&options)).WithName("agent"), nil
}

// run starts the agent and blocks until it is signalled or the RPC
// server fails. It stops the game gracefully before it returns.
func run(ctx context.Context, log logr.Logger, cfg agentConfig) error {
	gameAgent, err := agent.New(log, agent.Options{ //nolint:exhaustruct_v5 // The defaults fill the optional fields.
		SaveDir:  cfg.SaveDir,
		CacheDir: cfg.CacheDir,
	})
	if err != nil {
		return fmt.Errorf("could not create the agent: %w", err)
	}

	// The listener comes up before any game starts, so a pod whose
	// address is unusable never starts a game it would have to save.
	listener, err := listen(ctx, cfg.ListenAddress)
	if err != nil {
		return err
	}

	gameAgent.Resume(ctx)

	serveError := make(chan error, 1)

	go func() {
		// Serve returns with net.ErrClosed once the listener is closed.
		serveError <- capnprpc.Serve(listener, capnp.Client(rpcapi.Agent_ServerToClient(gameAgent)))
	}()

	log.Info("Started agent", "version", version.Agent,
		"listen", cfg.ListenAddress, "saveDir", cfg.SaveDir, "cacheDir", cfg.CacheDir)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	serveErr := waitForSignal(log, signals, serveError)

	return shutdown(ctx, log, gameAgent, listener, serveError, serveErr)
}

// waitForSignal blocks until the agent is signalled or the RPC server
// fails, and returns the server error when it failed.
func waitForSignal(log logr.Logger, signals <-chan os.Signal, serveError <-chan error) error {
	select {
	case sig := <-signals:
		log.Info("Received signal, stopping the game gracefully", "signal", sig.String())

		return nil
	case err := <-serveError:
		log.Error(err, "RPC server failed, stopping the game gracefully")

		return fmt.Errorf("the RPC server failed: %w", err)
	}
}

// listen opens the RPC listener.
func listen(ctx context.Context, listenAddress string) (net.Listener, error) {
	var listenerConfig net.ListenConfig

	listener, err := listenerConfig.Listen(ctx, "tcp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("could not listen on %s: %w", listenAddress, err)
	}

	return listener, nil
}

// shutdown closes the RPC listener, gracefully stops the game and waits
// for the RPC server to return. serveErr is the error the server
// already returned, or nil while it is still serving.
func shutdown(
	ctx context.Context,
	log logr.Logger,
	gameAgent *agent.Agent,
	listener net.Listener,
	serveError <-chan error,
	serveErr error,
) error {
	err := listener.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Error(err, "Could not close the RPC listener")
	}

	stopCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	stopErr := gameAgent.Shutdown(stopCtx)
	if stopErr != nil {
		log.Error(stopErr, "Could not stop the game gracefully")

		stopErr = fmt.Errorf("could not stop the game gracefully: %w", stopErr)
	}

	if serveErr == nil {
		// The server returns once the listener is closed.
		serveErr = <-serveError
		if errors.Is(serveErr, net.ErrClosed) {
			serveErr = nil
		}
	}

	return errors.Join(stopErr, serveErr)
}
