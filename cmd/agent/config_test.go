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
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes content to a temporary file named name and returns its
// path.
func writeConfig(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("Could not write the configuration file: %v", err)
	}

	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeConfig(t, "agent.yaml", ""))
	if err != nil {
		t.Fatalf("loadConfig returned an error: %v", err)
	}

	if cfg.ListenAddress != ":9550" {
		t.Errorf("loadConfig returned listenAddress %q, want %q", cfg.ListenAddress, ":9550")
	}

	if cfg.SaveDir != "/var/lib/koujouo" {
		t.Errorf("loadConfig returned saveDir %q, want %q", cfg.SaveDir, "/var/lib/koujouo")
	}

	if cfg.CacheDir != "/opt/koujouo" {
		t.Errorf("loadConfig returned cacheDir %q, want %q", cfg.CacheDir, "/opt/koujouo")
	}

	if cfg.TokenFile != "/var/run/koujouo/token" {
		t.Errorf("loadConfig returned tokenFile %q, want %q", cfg.TokenFile, "/var/run/koujouo/token")
	}

	if cfg.Log.Development || cfg.Log.Level != "info" {
		t.Errorf("loadConfig returned the log section %+v, want development=false level=info", cfg.Log)
	}
}

func TestLoadConfigFile(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeConfig(t, "agent.yaml", `listenAddress: "127.0.0.1:19550"
saveDir: /tmp/save
cacheDir: /tmp/cache
tokenFile: /tmp/token
log:
  development: false
  level: debug
`))
	if err != nil {
		t.Fatalf("loadConfig returned an error: %v", err)
	}

	if cfg.ListenAddress != "127.0.0.1:19550" {
		t.Errorf("loadConfig returned listenAddress %q, want %q", cfg.ListenAddress, "127.0.0.1:19550")
	}

	if cfg.SaveDir != "/tmp/save" || cfg.CacheDir != "/tmp/cache" {
		t.Errorf("loadConfig returned saveDir %q and cacheDir %q, want /tmp/save and /tmp/cache",
			cfg.SaveDir, cfg.CacheDir)
	}

	if cfg.TokenFile != "/tmp/token" {
		t.Errorf("loadConfig returned tokenFile %q, want %q", cfg.TokenFile, "/tmp/token")
	}

	if cfg.Log.Development || cfg.Log.Level != "debug" {
		t.Errorf("loadConfig returned the log section %+v, want development=false level=debug", cfg.Log)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	_, err := loadConfig(writeConfig(t, "agent.yaml", "listen: [\":9550\"\n"))
	if err == nil {
		t.Fatal("loadConfig accepted a malformed file")
	}

	_, err = loadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("loadConfig accepted a missing file")
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	_, err := newLogger(logConfig{Development: true, Level: "verbose"})
	if err == nil {
		t.Fatal("newLogger accepted an unknown level")
	}
}
