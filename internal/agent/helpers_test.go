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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	rpcapi "koujouo.anqou.net/project/rpc"
)

// Test helpers shared by the agent unit tests.
const (
	httpStatusNoContent     = http.StatusNoContent
	httpStatusInternalError = http.StatusInternalServerError
	httpStatusNotFound      = http.StatusNotFound
	testBackoff             = time.Millisecond

	// testGameVersion is the game version used across the agent tests.
	testGameVersion = "2.1.24"

	// testRconPassword is the RCON password used across the tests.
	testRconPassword = "cafebabe"

	// testPlayerKofuk is a player name of the log fixtures.
	testPlayerKofuk = "kofuk"

	// testConfigPath and testSavePath are the argument fixtures of the
	// game command line tests.
	testConfigPath = "/var/lib/koujouo/config.ini"
	testSavePath   = "/var/lib/koujouo/saves/map.zip"
)

// stringReader builds a reader of one string; it exists only to keep
// the test lines short.
func stringReader(content string) io.Reader {
	return strings.NewReader(content)
}

// testHTTPClient returns a client without redirects or timeouts that
// would slow the tests down.
func testHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// webhookRequest is one captured webhook post.
type webhookRequest struct {
	body        string
	contentType string
}

// webhookStatusOf returns the status of the call-th request, repeating
// the last one.
func webhookStatusOf(statuses []int, call int) int {
	status := statuses[len(statuses)-1]
	if call < len(statuses) {
		status = statuses[call]
	}

	return status
}

// newCapturingWebhook serves a Discord-like webhook that answers with
// the given statuses, one per request (the last one repeats), and
// captures the request bodies.
func newCapturingWebhook(t *testing.T, statuses ...int) (*httptest.Server, chan string) {
	t.Helper()

	requests := make(chan string, 16)

	call := 0

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("could not read the webhook request body: %v", err)
		}

		requests <- string(body)

		writer.WriteHeader(webhookStatusOf(statuses, call))

		call++
	}))

	t.Cleanup(server.Close)

	return server, requests
}

// newCapturingWebhookDetailed also captures the content type of the
// requests.
func newCapturingWebhookDetailed(t *testing.T, statuses ...int) (*httptest.Server, chan webhookRequest) {
	t.Helper()

	requests := make(chan webhookRequest, 16)

	call := 0

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("could not read the webhook request body: %v", err)
		}

		requests <- webhookRequest{body: string(body), contentType: request.Header.Get("Content-Type")}

		writer.WriteHeader(webhookStatusOf(statuses, call))

		call++
	}))

	t.Cleanup(server.Close)

	return server, requests
}

// discordMessage returns the JSON body of a Discord notification.
func discordMessage(content string) string {
	return `{"content":"` + content + `","username":"Factorio"}`
}

// testNotifier returns a notifier for the test webhook without a
// noticeable retry backoff.
func testNotifier(webhookURL string) *notifier {
	notify := newNotifier(webhookURL, testHTTPClient())
	notify.backoff = testBackoff

	return notify
}

// assertNotification waits for one notification and compares it.
func assertNotification(t *testing.T, requests chan string, want string) {
	t.Helper()

	select {
	case got := <-requests:
		if got != want {
			t.Errorf("notification body = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("notification %q never arrived", want)
	}
}

// logSink records what an agent logs, so the tests can assert on the
// mirrored game output.
type logSink struct {
	mutex sync.Mutex
	lines []string
}

// newLogSink returns a logger that records its messages.
func newLogSink() (logr.Logger, *logSink) {
	sink := &logSink{}

	return logr.New(sink), sink
}

// Init implements logr.LogSink.
func (s *logSink) Init(logr.RuntimeInfo) {}

// Enabled implements logr.LogSink.
func (s *logSink) Enabled(int) bool { return true }

// Info implements logr.LogSink.
func (s *logSink) Info(_ int, msg string, keysAndValues ...any) {
	s.record("INFO "+msg, keysAndValues)
}

// Error implements logr.LogSink.
func (s *logSink) Error(err error, msg string, keysAndValues ...any) {
	s.record("ERROR "+msg+" ("+err.Error()+")", keysAndValues)
}

// WithValues implements logr.LogSink.
func (s *logSink) WithValues(keysAndValues ...any) logr.LogSink { //nolint:ireturn // LogSink requires it.
	s.record("VALUES", keysAndValues)

	return s
}

// WithName implements logr.LogSink.
func (s *logSink) WithName(name string) logr.LogSink { //nolint:ireturn // LogSink requires it.
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.lines = append(s.lines, "NAME "+name)

	return s
}

// record appends one rendered message.
func (s *logSink) record(msg string, keysAndValues []any) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.lines = append(s.lines, msg+" "+renderPairs(keysAndValues))
}

// renderPairs renders log key-value pairs for the assertions.
func renderPairs(keysAndValues []any) string {
	pairs := make([]string, 0, len(keysAndValues))

	for index := 0; index+1 < len(keysAndValues); index += 2 {
		pairs = append(pairs, fmt.Sprint(keysAndValues[index])+"="+fmt.Sprint(keysAndValues[index+1]))
	}

	return strings.Join(pairs, " ")
}

// waitForLog returns the first recorded line holding part, waiting for
// it to show up.
func (s *logSink) waitForLog(t *testing.T, part string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		if line, found := s.find(part); found {
			return line
		}

		if time.Now().After(deadline) {
			t.Fatalf("no log line holds %q in\n%s", part, strings.Join(s.all(), "\n"))

			return ""
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// waitForCount waits until count recorded lines hold part.
func (s *logSink) waitForCount(t *testing.T, part string, count int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for s.count(part) < count {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d log lines hold %q in\n%s", s.count(part), count, part, strings.Join(s.all(), "\n"))

			return
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// assertNoLog fails when a recorded line holds part.
func (s *logSink) assertNoLog(t *testing.T, part string) {
	t.Helper()

	if line, found := s.find(part); found {
		t.Errorf("unexpected log line %q", line)
	}
}

// find returns the first recorded line holding part.
func (s *logSink) find(part string) (string, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, line := range s.lines {
		if strings.Contains(line, part) {
			return line, true
		}
	}

	return "", false
}

// count returns how many recorded lines hold part.
func (s *logSink) count(part string) int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	found := 0

	for _, line := range s.lines {
		if strings.Contains(line, part) {
			found++
		}
	}

	return found
}

// all returns a copy of the recorded lines.
func (s *logSink) all() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return append([]string{}, s.lines...)
}

// gameStatus is the answer of one status call.
type gameStatus struct {
	phase        rpcapi.Phase
	gameVersion  string
	players      uint16
	agentVersion string
}

// statusOf reads the agent status over the wire without failing the
// test, so it can run on a goroutine of its own.
func statusOf(ctx context.Context, client rpcapi.Agent) (gameStatus, error) {
	future, release := client.Status(ctx, nil)
	defer release()

	results, err := future.Struct()
	if err != nil {
		return gameStatus{}, fmt.Errorf("status call failed: %w", err)
	}

	status, err := results.St()
	if err != nil {
		return gameStatus{}, fmt.Errorf("could not read the agent status: %w", err)
	}

	gameVersion, err := status.GameVersion()
	if err != nil {
		return gameStatus{}, fmt.Errorf("could not read the game version: %w", err)
	}

	agentVersion, err := status.AgentVersion()
	if err != nil {
		return gameStatus{}, fmt.Errorf("could not read the agent version: %w", err)
	}

	return gameStatus{
		phase:        status.Phase(),
		gameVersion:  gameVersion,
		players:      status.Players(),
		agentVersion: agentVersion,
	}, nil
}

// stopOf asks the agent to stop the game over the wire without failing
// the test.
func stopOf(ctx context.Context, client rpcapi.Agent) error {
	future, release := client.Stop(ctx, nil)
	defer release()

	_, err := future.Struct()
	if err != nil {
		return fmt.Errorf("stop call failed: %w", err)
	}

	return nil
}
