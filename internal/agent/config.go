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

// Package agent implements the koujouo agent: the PID 1 process of the
// Factorio StatefulSet pod that manages the game lifecycle and serves
// the Cap'n Proto RPC protocol the controller talks to.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// stateFileName is the name of the cached state file in the save
// directory. It holds the last configuration received from the
// controller, so a restarted pod can resume the game without waiting
// for the next configure call.
const stateFileName = "agent-state.json"

// configFileIniName and serverWhitelistFileName are the file names the
// controller delivers in Config.files. They match the constants in the
// controller package.
const (
	configFileIniName      = "config.ini"
	serverWhitelistName    = "server-whitelist.json"
	writeDataSectionHeader = "[path]"
	writeDataKey           = "write-data"
)

// Config is the full desired state of one agent, as sent on every
// configure call and cached in the state file.
type Config struct {
	GameVersion         string          `json:"gameVersion"`
	Files               []ConfigFile    `json:"files,omitempty"`
	DiscordWebhookURL   string          `json:"discordWebhookUrl,omitempty"`
	LeaveTimeoutSeconds int32           `json:"leaveTimeoutSeconds"`
	Backup              *BackupSettings `json:"backup,omitempty"`
	DownloadUsername    string          `json:"downloadUsername,omitempty"`
	DownloadToken       string          `json:"downloadToken,omitempty"`
}

// ConfigFile is one file delivered to the agent, for example config.ini.
type ConfigFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// BackupSettings configures the periodic save upload. A nil pointer
// disables backups.
type BackupSettings struct {
	EndpointURL     string `json:"endpointUrl"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

// persistedState is the JSON layout of the state file.
type persistedState struct {
	Config       *Config `json:"config,omitempty"`
	RconPort     int     `json:"rconPort,omitempty"`
	RconPassword string  `json:"rconPassword,omitempty"`
}

// configsEqual reports whether two configurations carry the same
// content, which makes a configure call a no-op.
func configsEqual(left, right *Config) bool {
	return reflect.DeepEqual(left, right)
}

// fileContent returns the content of the named delivered file.
func (c *Config) fileContent(name string) (string, bool) {
	for i := range c.Files {
		if c.Files[i].Name == name {
			return c.Files[i].Content, true
		}
	}

	return "", false
}

// hasServerWhitelist reports whether the configuration delivers a
// server whitelist file.
func (c *Config) hasServerWhitelist() bool {
	_, ok := c.fileContent(serverWhitelistName)

	return ok
}

// saveState writes the state file atomically into dir.
func saveState(dir string, state *persistedState) error {
	err := os.MkdirAll(dir, directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the save directory: %w", err)
	}

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("could not encode the agent state: %w", err)
	}

	return writeFileSync(filepath.Join(dir, stateFileName), encoded, privateFileMode)
}

// loadState reads the state file from dir. It returns nil when no state
// file exists.
func loadState(dir string) (*persistedState, error) {
	encoded, err := os.ReadFile(filepath.Join(dir, stateFileName)) //nolint:gosec // The path is built by the agent itself.
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // No state file means a fresh agent.
	}

	if err != nil {
		return nil, fmt.Errorf("could not read the agent state: %w", err)
	}

	state := &persistedState{} //nolint:exhaustruct_v5 // Filled by unmarshal.

	err = json.Unmarshal(encoded, state)
	if err != nil {
		return nil, fmt.Errorf("could not decode the agent state: %w", err)
	}

	return state, nil
}

// writeFileSync writes data to path atomically (via a temporary file in
// the same directory) and fsyncs it.
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary file for %s: %w", path, err)
	}

	temporaryName := temporary.Name()

	err = func() error {
		defer temporary.Close() //nolint:errcheck // Nothing useful to do on close of a temp file.

		_, err := temporary.Write(data)
		if err != nil {
			return fmt.Errorf("could not write %s: %w", path, err)
		}

		err = temporary.Chmod(mode)
		if err != nil {
			return fmt.Errorf("could not set the mode of %s: %w", temporaryName, err)
		}

		err = temporary.Sync()
		if err != nil {
			return fmt.Errorf("could not sync %s: %w", temporaryName, err)
		}

		return nil
	}()
	if err != nil {
		_ = os.Remove(temporaryName)

		return err
	}

	err = os.Rename(temporaryName, path)
	if err != nil {
		_ = os.Remove(temporaryName)

		return fmt.Errorf("could not replace %s: %w", path, err)
	}

	return nil
}

// rewriteWriteData rewrites the write-data key of the [path] section of
// a config.ini content to dir, so the game writes its data (saves,
// mods, player data) into the save volume. A missing key or section is
// inserted into (or as) the [path] section.
func rewriteWriteData(content, dir string) string {
	lines := trimTrailingEmptyLine(strings.Split(content, "\n"))

	inPathSection := false

	sectionFound := false

	keyFound := false

	// insertAt is the index behind the last line of the [path]
	// section, where a missing write-data key belongs.
	insertAt := 0

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inPathSection = trimmed == writeDataSectionHeader

			if inPathSection {
				sectionFound = true
				insertAt = index + 1
			}

			continue
		}

		if !inPathSection {
			continue
		}

		insertAt = index + 1

		key, _, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(key) != writeDataKey {
			continue
		}

		indentation := line[:len(line)-len(strings.TrimLeft(line, " \t"))]

		lines[index] = indentation + writeDataKey + "=" + dir

		keyFound = true
	}

	if keyFound {
		return strings.Join(lines, "\n") + "\n"
	}

	if !sectionFound {
		lines = append(lines, writeDataSectionHeader)

		insertAt = len(lines)
	}

	return strings.Join(insertLine(lines, insertAt, writeDataKey+"="+dir), "\n") + "\n"
}

// trimTrailingEmptyLine drops one trailing empty line of a split
// document, so appended lines keep the trailing newline structure.
func trimTrailingEmptyLine(lines []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}

	return lines
}

// insertLine inserts line at index, shifting the rest back.
func insertLine(lines []string, index int, line string) []string {
	lines = append(lines, "")

	copy(lines[index+1:], lines[index:])

	lines[index] = line

	return lines
}
