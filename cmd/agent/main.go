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
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	capnprpc "capnproto.org/go/capnp/v3/rpc"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"koujouo.anqou.net/project/internal/agent"
	"koujouo.anqou.net/project/internal/version"
	rpcapi "koujouo.anqou.net/project/rpc"
)

// Agent defaults. The shutdown deadline stays below the 600 seconds
// termination grace period of the StatefulSet, so the graceful stop
// finishes before the kubelet would kill the pod.
const (
	defaultListenAddress = ":9550"
	defaultSaveDir       = "/var/lib/koujouo"
	defaultCacheDir      = "/opt/koujouo"
	shutdownTimeout      = 9 * time.Minute
)

// environment variable names overriding the defaults of the flags.
const (
	envListen   = "KOJOUO_LISTEN"
	envSaveDir  = "KOJOUO_SAVE_DIR"
	envCacheDir = "KOJOUO_CACHE_DIR"
)

// envDefault returns the value of the environment variable name, or
// fallback when it is unset or empty.
func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func main() {
	listenAddress := flag.String("listen", envDefault(envListen, defaultListenAddress),
		"address to serve the Cap'n Proto RPC protocol on")
	saveDir := flag.String("save-dir", envDefault(envSaveDir, defaultSaveDir),
		"game write-data directory on the save volume")
	cacheDir := flag.String("cache-dir", envDefault(envCacheDir, defaultCacheDir),
		"game binary and upgrade cache directory on the ephemeral volume")

	flag.Parse()

	log := zap.New().WithName("agent")

	err := run(context.Background(), log, *listenAddress, *saveDir, *cacheDir)
	if err != nil {
		log.Error(err, "Could not run the agent")
		os.Exit(1)
	}

	log.Info("Stopped agent")
}

// run starts the agent and blocks until it is signalled or the RPC
// server fails. It stops the game gracefully before it returns.
func run(ctx context.Context, log logr.Logger, listenAddress, saveDir, cacheDir string) error {
	gameAgent, err := agent.New(log, agent.Options{ //nolint:exhaustruct_v5 // The defaults fill the optional fields.
		SaveDir:  saveDir,
		CacheDir: cacheDir,
	})
	if err != nil {
		return fmt.Errorf("could not create the agent: %w", err)
	}

	// The listener comes up before any game starts, so a pod whose
	// address is unusable never starts a game it would have to save.
	listener, err := listen(ctx, listenAddress)
	if err != nil {
		return err
	}

	gameAgent.Resume(ctx)

	serveError := make(chan error, 1)

	go func() {
		// Serve returns with net.ErrClosed once the listener is closed.
		serveError <- capnprpc.Serve(listener, capnp.Client(rpcapi.Agent_ServerToClient(gameAgent)))
	}()

	log.Info("Started agent", "version", version.Agent, "listen", listenAddress, "saveDir", saveDir, "cacheDir", cacheDir)

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
