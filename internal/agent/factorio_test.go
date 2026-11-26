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

package agent //nolint:testpackage // White-box tests exercise the agent internals.

import (
	"slices"
	"testing"
)

// Fixtures of the game command line tests.
const (
	testBinary        = "/opt/koujouo/factorio-2.1.24/bin/x64/factorio"
	testWhitelistPath = "/var/lib/koujouo/server-whitelist.json"
	testRconBind      = "127.0.0.1:55555"
)

// baseStartParams returns the run parameters the cases mutate.
func baseStartParams() startParams {
	return startParams{
		binary:        testBinary,
		configPath:    testConfigPath,
		savePath:      testSavePath,
		whitelistPath: "",
		rconPort:      rconDefaultPort,
		rconPassword:  testRconPassword,
		create:        false,
	}
}

// TestFactorioArgsStartServer verifies the serving command line with
// and without a server whitelist.
func TestFactorioArgsStartServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		whitelistPath string
		want          []string
	}{
		{
			name:          "without a whitelist",
			whitelistPath: "",
			want: []string{
				"-c", testConfigPath,
				"--start-server", testSavePath,
				"--rcon-bind", testRconBind,
				"--rcon-password", testRconPassword,
			},
		},
		{
			name:          "with a whitelist",
			whitelistPath: testWhitelistPath,
			want: []string{
				"-c", testConfigPath,
				"--use-server-whitelist",
				"--server-whitelist", testWhitelistPath,
				"--start-server", testSavePath,
				"--rcon-bind", testRconBind,
				"--rcon-password", testRconPassword,
			},
		},
	}

	for _, test := range tests {
		params := baseStartParams()
		params.whitelistPath = test.whitelistPath

		got := factorioArgs(params)
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: factorioArgs() = %v, want %v", test.name, got, test.want)
		}
	}
}

// TestFactorioArgsCreate verifies that a create run omits the RCON
// arguments.
func TestFactorioArgsCreate(t *testing.T) {
	t.Parallel()

	params := baseStartParams()
	params.create = true

	want := []string{"-c", testConfigPath, "--create", testSavePath}

	got := factorioArgs(params)
	if !slices.Equal(got, want) {
		t.Errorf("factorioArgs() = %v, want %v", got, want)
	}
}

// TestGameBinaryPath verifies the binary path layout of a version.
func TestGameBinaryPath(t *testing.T) {
	t.Parallel()

	got := gameBinaryPath("/opt/koujouo", testGameVersion)

	want := "/opt/koujouo/factorio-2.1.24/bin/x64/factorio"
	if got != want {
		t.Errorf("gameBinaryPath() = %q, want %q", got, want)
	}
}
