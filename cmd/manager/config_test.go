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
	"reflect"
	"testing"
)

// defaultTestConfig is the configuration loadConfig returns for a file
// without keys.
func defaultTestConfig() managerConfig {
	return managerConfig{
		MetricsBindAddress:     "0",
		HealthProbeBindAddress: ":8081",
		LeaderElection:         false,
		SecureMetrics:          true,
		WebhookCertPath:        "",
		WebhookCertName:        "tls.crt",
		WebhookCertKey:         "tls.key",
		MetricsCertPath:        "",
		MetricsCertName:        "tls.crt",
		MetricsCertKey:         "tls.key",
		ControllerImage:        "",
		Log:                    logConfig{Development: true, Level: "info"},
	}
}

// writeConfig writes content to a temporary file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "manager.yaml")

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("Could not write the configuration file: %v", err)
	}

	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("loadConfig returned an error: %v", err)
	}

	want := defaultTestConfig()

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("loadConfig returned %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigPartialFile(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeConfig(t, `leaderElection: true
log:
  level: warn
`))
	if err != nil {
		t.Fatalf("loadConfig returned an error: %v", err)
	}

	want := defaultTestConfig()
	want.LeaderElection = true
	want.Log.Level = "warn"

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("loadConfig returned %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigEveryKey(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeConfig(t, `metricsBindAddress: ":8443"
healthProbeBindAddress: ":8082"
leaderElection: true
secureMetrics: false
webhookCertPath: /certs/webhook
webhookCertName: webhook.crt
webhookCertKey: webhook.key
metricsCertPath: /certs/metrics
metricsCertName: metrics.crt
metricsCertKey: metrics.key
controllerImage: ghcr.io/ushitora-anqou/koujouo:latest
log:
  development: false
  level: debug
`))
	if err != nil {
		t.Fatalf("loadConfig returned an error: %v", err)
	}

	want := managerConfig{
		MetricsBindAddress:     ":8443",
		HealthProbeBindAddress: ":8082",
		LeaderElection:         true,
		SecureMetrics:          false,
		WebhookCertPath:        "/certs/webhook",
		WebhookCertName:        "webhook.crt",
		WebhookCertKey:         "webhook.key",
		MetricsCertPath:        "/certs/metrics",
		MetricsCertName:        "metrics.crt",
		MetricsCertKey:         "metrics.key",
		ControllerImage:        "ghcr.io/ushitora-anqou/koujouo:latest",
		Log:                    logConfig{Development: false, Level: "debug"},
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("loadConfig returned %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "malformed YAML", content: "leaderElection: [true\n"},
		{name: "unknown type", content: "leaderElection: sometimes\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadConfig(writeConfig(t, test.content))
			if err == nil {
				t.Fatalf("loadConfig accepted the content %q", test.content)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Parallel()

	_, err := loadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("loadConfig accepted a missing file")
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	_, err := newLogger(logConfig{Development: true, Level: "loud"})
	if err == nil {
		t.Fatal("newLogger accepted an unknown level")
	}
}
