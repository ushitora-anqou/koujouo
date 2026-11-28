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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// Timeout of the output reader tests, which must never hang on a game
// or a webhook that stopped responding.
const gameOutputTestTimeout = 5 * time.Second

// TestParsePlayerEvent verifies the game output parser against real
// Factorio log lines.
func TestParsePlayerEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line   string
		kind   playerEventKind
		player string
		ok     bool
	}{
		{line: "  2.222 [JOIN] anqou joined the game", kind: playerEventJoin, player: testPlayerAnqou, ok: true},
		{line: "2022-02-01 15:31:19 [JOIN] kofuk joined the game", kind: playerEventJoin, player: testPlayerKofuk, ok: true},
		{line: "  3.500 [LEAVE] anqou left the game", kind: playerEventLeave, player: testPlayerAnqou, ok: true},
		{line: "2022-02-01 15:40:02 [LEAVE] kofuk left the game", kind: playerEventLeave, player: testPlayerKofuk, ok: true},
		{line: "  0.000 Info ServerMultiplayerManager.cpp: 0",
			kind: "", player: "", ok: false},
		{line: "  1.000 [CHAT] anqou: hello", kind: "", player: "", ok: false},
		{line: "  4.000 [LEAVE] kofuk left the game (map view)", kind: playerEventLeave, player: testPlayerKofuk, ok: true},
		{line: "garbage without any tag", kind: "", player: "", ok: false},
		{line: "", kind: "", player: "", ok: false},
	}

	for _, test := range tests {
		kind, player, ok := parsePlayerEvent(test.line)

		if kind != test.kind || player != test.player || ok != test.ok {
			t.Errorf("parsePlayerEvent(%q) = (%q, %q, %v), want (%q, %q, %v)",
				test.line, kind, player, ok, test.kind, test.player, test.ok)
		}
	}
}

// testPlayerAnqou is the other player name of the log fixtures.
const testPlayerAnqou = "anqou"

// gameOutputFixture holds the log lines the output reader test feeds.
const gameOutputFixture = "  0.000 Info ServerMultiplayerManager.cpp: 0\n" +
	"  2.222 [JOIN] anqou joined the game\n" +
	"  5.000 [LEAVE] anqou left the game\n"

// TestNotificationOfPlayerEvent verifies the Discord messages of the
// player events.
func TestNotificationOfPlayerEvent(t *testing.T) {
	t.Parallel()

	if got, want := notificationOfPlayerEvent(playerEventJoin, testPlayerAnqou), "anqou joined the game"; got != want {
		t.Errorf("notificationOfPlayerEvent(join) = %q, want %q", got, want)
	}

	want := testPlayerKofuk + " left the game"
	if got := notificationOfPlayerEvent(playerEventLeave, testPlayerKofuk); got != want {
		t.Errorf("notificationOfPlayerEvent(leave) = %q, want %q", got, want)
	}
}

// TestCopyGameOutput notifies the player events of a game output
// stream.
func TestCopyGameOutput(t *testing.T) {
	t.Parallel()

	server, requests := newCapturingWebhook(t, httpStatusNoContent)

	notify := newNotifier(server.URL, testHTTPClient())
	notify.backoff = testBackoff

	copyGameOutput(t.Context(), logr.Discard(), stringReader(gameOutputFixture), streamStdout,
		func() *notifier { return notify })

	assertNotification(t, requests, discordMessage("anqou joined the game"))
	assertNotification(t, requests, discordMessage("anqou left the game"))

	select {
	case extra := <-requests:
		t.Errorf("unexpected extra notification %q", extra)
	default:
	}
}

// TestCopyGameOutputUsesTheCurrentNotifier verifies that the output
// reader looks the notifier up for every event, so a webhook that is
// switched while the game runs applies to the following events.
func TestCopyGameOutputUsesTheCurrentNotifier(t *testing.T) {
	t.Parallel()

	first, firstRequests := newCapturingWebhook(t, httpStatusNoContent)
	second, secondRequests := newCapturingWebhook(t, httpStatusNoContent)

	gameAgent, err := New(logr.Discard(), Options{
		SaveDir:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("could not create the agent: %v", err)
	}

	gameAgent.setNotifier(testNotifier(first.URL))

	reader, writer := io.Pipe()

	streamDone := make(chan struct{})

	go func() {
		defer close(streamDone)

		copyGameOutput(t.Context(), logr.Discard(), reader, streamStdout, gameAgent.currentNotifier)
	}()

	_, err = io.WriteString(writer, "  2.222 [JOIN] anqou joined the game\n")
	if err != nil {
		t.Fatalf("could not write the first line: %v", err)
	}

	assertNotification(t, firstRequests, discordMessage("anqou joined the game"))

	gameAgent.setNotifier(testNotifier(second.URL))

	_, err = io.WriteString(writer, "  3.000 [JOIN] kofuk joined the game\n")
	if err != nil {
		t.Fatalf("could not write the second line: %v", err)
	}

	_ = writer.Close()

	<-streamDone

	assertNotification(t, secondRequests, discordMessage(testPlayerKofuk+" joined the game"))

	select {
	case extra := <-firstRequests:
		t.Errorf("the first webhook received an event of the switched notifier: %q", extra)
	default:
	}
}

// longGameOutputLineLength is the length of the single line the long
// line test streams. It is well above the 64 KiB default limit of
// bufio.Scanner that stopped the reader before.
const longGameOutputLineLength = 128 * 1024

// TestCopyGameOutputHandlesLongLines verifies that one line beyond the
// default scanner limit neither stops the reader nor blocks the game
// writing its output.
func TestCopyGameOutputHandlesLongLines(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()

	streamDone := make(chan struct{})

	go func() {
		defer close(streamDone)

		copyGameOutput(t.Context(), logr.Discard(), reader, streamStdout, func() *notifier { return nil })
	}()

	writeErr := make(chan error, 1)

	go func() {
		_, err := io.WriteString(writer, strings.Repeat("x", longGameOutputLineLength)+"\n")
		if err != nil {
			writeErr <- err

			return
		}

		writeErr <- writer.Close()
	}()

	select {
	case <-streamDone:
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("copyGameOutput did not drain the long line")
	}

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("could not write the long line: %v", err)
		}
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("the write of the long line blocked, the reader stopped reading")
	}
}

// newStalledWebhook serves a webhook that answers only once the test
// releases it, so a notification sender blocks on it. The returned
// release function is idempotent.
func newStalledWebhook(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	release := make(chan struct{})

	var once sync.Once

	releaseWebhook := func() {
		once.Do(func() { close(release) })
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release

		writer.WriteHeader(httpStatusNoContent)
	}))

	// The webhook is released before the server closes (cleanups run
	// last in, first out), so the pending request does not hold the
	// shutdown up.
	t.Cleanup(server.Close)
	t.Cleanup(releaseWebhook)

	return server, releaseWebhook
}

// stalledWebhookLines is how many lines the slow webhook test writes
// after the first player event. Every other line carries a player
// event, so the notification queue overflows and the reader has to drop
// notifications instead of waiting for the webhook.
const stalledWebhookLines = 4 * gameOutputQueueLength

// TestCopyGameOutputDoesNotBlockOnASlowWebhook verifies that a webhook
// that does not answer never stops the reader: the game keeps writing
// its output and the dropped notifications are acceptable.
func TestCopyGameOutputDoesNotBlockOnASlowWebhook(t *testing.T) {
	t.Parallel()

	server, release := newStalledWebhook(t)

	notify := newNotifier(server.URL, testHTTPClient())
	notify.backoff = testBackoff

	reader, writer := io.Pipe()

	streamDone := make(chan struct{})

	go func() {
		defer close(streamDone)

		copyGameOutput(t.Context(), logr.Discard(), reader, streamStdout, func() *notifier { return notify })
	}()

	writeErr := make(chan error, 1)

	go func() {
		_, err := io.WriteString(writer, "  2.222 [JOIN] anqou joined the game\n")
		for index := range stalledWebhookLines {
			if err != nil {
				break
			}

			line := "  3.000 Info ServerMultiplayerManager.cpp: 0\n"
			if index%2 == 0 {
				line = "  3.500 [JOIN] kofuk joined the game\n"
			}

			_, err = io.WriteString(writer, line)
		}

		if err == nil {
			err = writer.Close()
		}

		writeErr <- err
	}()

	// Every write of the game completes only once the reader consumed
	// it; a reader waiting for the webhook would stall here.
	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("could not write the game output: %v", err)
		}
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("copyGameOutput stopped reading while the webhook did not answer")
	}

	// Let the sender finish, so no goroutine outlives the test.
	release()

	select {
	case <-streamDone:
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("copyGameOutput did not return once the webhook answered")
	}
}

// overlongGameOutputLineLength is the length of a line the scanner
// refuses: it is above the 1 MiB limit of the output reader.
const overlongGameOutputLineLength = 2 * gameOutputMaxLineLength

// TestCopyGameOutputDiscardsTheRestOfAnOverlongStream verifies that a
// line the scanner refuses leaves neither the rest of the line nor the
// rest of the stream unread, which would block the game writing it.
func TestCopyGameOutputDiscardsTheRestOfAnOverlongStream(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()

	streamDone := make(chan struct{})

	go func() {
		defer close(streamDone)

		copyGameOutput(t.Context(), logr.Discard(), reader, streamStdout, func() *notifier { return nil })
	}()

	writeErr := make(chan error, 1)

	go func() {
		_, err := io.WriteString(writer, strings.Repeat("x", overlongGameOutputLineLength)+"\n")
		if err == nil {
			err = writer.Close()
		}

		writeErr <- err
	}()

	select {
	case <-streamDone:
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("copyGameOutput left the rest of the overlong line unread")
	}

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("could not write the overlong line: %v", err)
		}
	case <-time.After(gameOutputTestTimeout):
		t.Fatal("the write of the overlong line blocked, the reader stopped reading")
	}
}
