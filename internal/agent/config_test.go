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
	"strings"
	"testing"
)

// TestRewriteWriteData verifies the write-data rewrite of the delivered
// config.ini.
func TestRewriteWriteData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "replaces the existing write-data",
			content: "[path]\nread-data=__PATH__executable__/../../data\nwrite-data=/old\n",
			want:    "[path]\nread-data=__PATH__executable__/../../data\nwrite-data=/var/lib/koujouo\n",
		},
		{
			name:    "appends the key to an existing path section",
			content: "[path]\nread-data=__PATH__executable__/../../data\n",
			want:    "[path]\nread-data=__PATH__executable__/../../data\nwrite-data=/var/lib/koujouo\n",
		},
		{
			name:    "appends the section when missing",
			content: "[other]\nkey=value\n",
			want:    "[other]\nkey=value\n[path]\nwrite-data=/var/lib/koujouo\n",
		},
		{
			name:    "keeps the indentation of the replaced line",
			content: "[path]\n  write-data=/old\n",
			want:    "[path]\n  write-data=/var/lib/koujouo\n",
		},
		{
			name:    "inserts into a path section followed by another section",
			content: "[path]\nread-data=x\n[other]\nkey=v\n",
			want:    "[path]\nread-data=x\nwrite-data=/var/lib/koujouo\n[other]\nkey=v\n",
		},
	}

	for _, test := range tests {
		got := rewriteWriteData(test.content, "/var/lib/koujouo")
		if got != test.want {
			t.Errorf("%s: rewriteWriteData() = %q, want %q", test.name, got, test.want)
		}
	}
}

// TestRewriteWriteDataOtherSections verifies that write-data keys
// outside the [path] section stay untouched.
func TestRewriteWriteDataOtherSections(t *testing.T) {
	t.Parallel()

	content := "[section]\nwrite-data=/keep\n[path]\nwrite-data=/old\n"

	got := rewriteWriteData(content, "/new")

	if !strings.Contains(got, "[section]\nwrite-data=/keep\n") {
		t.Errorf("rewriteWriteData() = %q, want the foreign write-data kept", got)
	}

	if !strings.Contains(got, "write-data=/new\n") {
		t.Errorf("rewriteWriteData() = %q, want the path write-data replaced", got)
	}
}

// TestStateRoundTrip verifies the persisted state file round trip.
func TestStateRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	state := &persistedState{
		Config: &Config{ //nolint:exhaustruct_v5 // The optional settings stay disabled.
			GameVersion: testGameVersion,
			Files:       []ConfigFile{{Name: configFileIniName, Content: "[path]\nwrite-data=/var/lib/koujouo\n"}},
			Backup: &BackupSettings{ //nolint:exhaustruct_v5 // The round trip also covers a missing key.
				EndpointURL: "https://s3.us-west-00.backblazeb2.com",
				Bucket:      "example",
				AccessKeyID: "key-id",
			},
			LeaveTimeoutSeconds: 120,
		},
		RconPort:     55555,
		RconPassword: testRconPassword,
	}

	err := saveState(dir, state)
	if err != nil {
		t.Fatalf("saveState() error = %v", err)
	}

	loaded, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}

	if !configsEqual(loaded.Config, state.Config) {
		t.Errorf("loadState().Config = %+v, want %+v", loaded.Config, state.Config)
	}

	if loaded.RconPort != state.RconPort || loaded.RconPassword != state.RconPassword {
		t.Errorf("loadState() rcon = (%d, %q), want (%d, %q)",
			loaded.RconPort, loaded.RconPassword, state.RconPort, state.RconPassword)
	}
}

// TestLoadStateMissing verifies that a missing state file loads as nil.
func TestLoadStateMissing(t *testing.T) {
	t.Parallel()

	loaded, err := loadState(t.TempDir())
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}

	if loaded != nil {
		t.Errorf("loadState() = %+v, want nil", loaded)
	}
}

// TestConfigsEqual verifies the configuration comparison behind the
// configure idempotency.
func TestConfigsEqual(t *testing.T) {
	t.Parallel()

	base := &Config{GameVersion: testGameVersion, LeaveTimeoutSeconds: 60} //nolint:exhaustruct_v5 // Compared fields only.

	same := &Config{GameVersion: testGameVersion, LeaveTimeoutSeconds: 60} //nolint:exhaustruct_v5 // Compared fields only.

	otherVersion := &Config{ //nolint:exhaustruct_v5 // Compared fields only.
		GameVersion:         testNewGameVersion,
		LeaveTimeoutSeconds: 60,
	}

	otherFiles := &Config{ //nolint:exhaustruct_v5 // Compared fields only.
		GameVersion:         testGameVersion,
		LeaveTimeoutSeconds: 60,
		Files:               []ConfigFile{{Name: configFileIniName, Content: "x"}},
	}

	if !configsEqual(base, same) {
		t.Error("configsEqual(base, same) = false, want true")
	}

	if configsEqual(base, otherVersion) {
		t.Error("configsEqual(base, otherVersion) = true, want false")
	}

	if configsEqual(base, otherFiles) {
		t.Error("configsEqual(base, otherFiles) = true, want false")
	}
}
