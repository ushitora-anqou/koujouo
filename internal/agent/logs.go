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
	"bufio"
	"context"
	"io"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
)

// playerEventKind describes a parsed player event of the game output.
type playerEventKind string

// Kinds of the parsed player events.
const (
	playerEventJoin  playerEventKind = "join"
	playerEventLeave playerEventKind = "leave"
)

// Names of the game output streams in the agent log.
const (
	streamStdout = "stdout"
	streamStderr = "stderr"
)

// The game output formats are verified against real Factorio logs in
// the unit tests. The expressions are unanchored because the lines
// carry a timestamp prefix.
var (
	reJoin  = regexp.MustCompile(`\[JOIN\] (\S+) joined the game`)
	reLeave = regexp.MustCompile(`\[LEAVE\] (\S+) left the game`)
)

// parsePlayerEvent extracts the player event of one game output line.
// It reports ok == false for every other line.
func parsePlayerEvent(line string) (playerEventKind, string, bool) {
	if match := reJoin.FindStringSubmatch(line); match != nil {
		return playerEventJoin, match[1], true
	}

	if match := reLeave.FindStringSubmatch(line); match != nil {
		return playerEventLeave, match[1], true
	}

	return "", "", false
}

// notificationOfPlayerEvent returns the Discord message of an event.
func notificationOfPlayerEvent(kind playerEventKind, player string) string {
	if kind == playerEventJoin {
		return player + " joined the game"
	}

	return player + " left the game"
}

// copyGameOutput reads the game output line by line, mirrors it to the
// agent log and forwards player events to the notifier. The notifier is
// looked up for every event, so a configuration that changes only the
// webhook applies to the running game. The function returns when the
// stream ends, which happens when the game process exits.
func copyGameOutput(
	ctx context.Context,
	log logr.Logger,
	reader io.Reader,
	stream string,
	notifier func() *notifier,
) {
	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		log.Info("Factorio output", "stream", stream, "line", line)

		kind, player, ok := parsePlayerEvent(line)
		if !ok {
			continue
		}

		err := notifier().Send(ctx, notificationOfPlayerEvent(kind, player))
		if err != nil {
			log.Error(err, "Could not notify Discord", "player", player)
		}
	}

	err := scanner.Err()
	if err != nil {
		log.Error(err, "Could not read the Factorio output", "stream", stream)
	}
}
