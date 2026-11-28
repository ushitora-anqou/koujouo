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
	"sync"

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

// Buffer and queue sizes of the game output reader. The scanner starts
// with a 64 KiB buffer and accepts lines up to 1 MiB, which covers the
// long lines a mod or a stack trace prints. The notification queue
// decouples the reader from the webhook: a full queue drops the
// notification instead of slowing the reader down.
const (
	gameOutputBufferLength  = 64 * 1024
	gameOutputMaxLineLength = 1024 * 1024
	gameOutputQueueLength   = 64
)

// The game output formats are verified against real Factorio logs in
// the unit tests. The expressions are unanchored because the lines
// carry a timestamp prefix.
var (
	reJoin  = regexp.MustCompile(`\[JOIN\] (\S+) joined the game`)
	reLeave = regexp.MustCompile(`\[LEAVE\] (\S+) left the game`)
)

// gameOutputNotification is one Discord message the output reader
// queued for the notification sender.
type gameOutputNotification struct {
	player  string
	message string
}

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
//
// Reading never waits for the webhook and never stops before the end of
// the stream: a game that cannot write to a full output pipe stops
// serving. The notifications therefore go through a queue and a sender
// goroutine of its own.
func copyGameOutput(
	ctx context.Context,
	log logr.Logger,
	reader io.Reader,
	stream string,
	notifier func() *notifier,
) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, gameOutputBufferLength), gameOutputMaxLineLength)

	events := make(chan gameOutputNotification, gameOutputQueueLength)

	var sender sync.WaitGroup

	// The sender drains the queue after the end of the stream too, so
	// the caller can wait for it.
	sender.Go(func() {
		sendGameOutputNotifications(context.WithoutCancel(ctx), log, events, notifier)
	})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		log.Info("Factorio output", "stream", stream, "line", line)

		kind, player, ok := parsePlayerEvent(line)
		if !ok {
			continue
		}

		select {
		case events <- gameOutputNotification{player: player, message: notificationOfPlayerEvent(kind, player)}:
		default:
			// A slow or broken webhook must not hold the game output,
			// so the reader drops the notification instead.
			log.Info("Dropped a Discord notification", "player", player, "queue", gameOutputQueueLength)
		}
	}

	err := scanner.Err()
	if err != nil {
		log.Error(err, "Could not read the Factorio output", "stream", stream)
	}

	close(events)

	// The scanner gives up on a line longer than its buffer; the rest
	// of the stream still has to go somewhere, or the game blocks.
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		log.Error(err, "Could not discard the rest of the Factorio output", "stream", stream)
	}

	sender.Wait()
}

// sendGameOutputNotifications posts the queued player events until the
// queue is closed.
func sendGameOutputNotifications(
	ctx context.Context,
	log logr.Logger,
	events <-chan gameOutputNotification,
	notifier func() *notifier,
) {
	for event := range events {
		err := notifier().Send(ctx, event.message)
		if err != nil {
			log.Error(err, "Could not notify Discord", "player", event.player)
		}
	}
}
