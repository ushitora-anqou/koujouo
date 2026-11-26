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
	"testing"
)

// TestNotifierSendPostsBody verifies the request a Discord webhook
// receives: the JSON body with the Factorio username and the JSON
// content type.
func TestNotifierSendPostsBody(t *testing.T) {
	t.Parallel()

	server, requests := newCapturingWebhookDetailed(t, httpStatusNoContent)
	defer closeAndDrain(t, requests)

	notify := newNotifier(server.URL, testHTTPClient())
	notify.backoff = testBackoff

	err := notify.Send(t.Context(), "anqou joined the game")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	select {
	case request := <-requests:
		wantBody := `{"content":"anqou joined the game","username":"Factorio"}`
		if request.body != wantBody {
			t.Errorf("request body = %q, want %q", request.body, wantBody)
		}

		if request.contentType != "application/json" {
			t.Errorf("request content type = %q, want %q", request.contentType, "application/json")
		}
	default:
		t.Fatal("the webhook received no request")
	}
}

// TestNotifierSendRetries verifies the retry on a 5xx response.
func TestNotifierSendRetries(t *testing.T) {
	t.Parallel()

	server, requests := newCapturingWebhook(t,
		httpStatusInternalError, httpStatusInternalError, httpStatusNoContent)
	defer closeAndDrain(t, requests)

	notify := newNotifier(server.URL, testHTTPClient())
	notify.backoff = testBackoff

	err := notify.Send(t.Context(), "anqou left the game")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	for range 3 {
		assertNotification(t, requests, `{"content":"anqou left the game","username":"Factorio"}`)
	}
}

// TestNotifierSendGivesUp verifies that a webhook answering 4xx (other
// than 429) fails without retries.
func TestNotifierSendGivesUp(t *testing.T) {
	t.Parallel()

	server, requests := newCapturingWebhook(t, httpStatusNotFound)
	defer closeAndDrain(t, requests)

	notify := newNotifier(server.URL, testHTTPClient())
	notify.backoff = testBackoff

	err := notify.Send(t.Context(), "hello")
	if err == nil {
		t.Fatal("Send() error = nil, want an error")
	}

	assertNotification(t, requests, `{"content":"hello","username":"Factorio"}`)

	select {
	case extra := <-requests:
		t.Errorf("unexpected retried request %q", extra)
	default:
	}
}

// TestNilNotifierSend verifies that a disabled notifier is a no-op.
func TestNilNotifierSend(t *testing.T) {
	t.Parallel()

	var notify *notifier

	err := notify.Send(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

// closeAndDrain drains leftover captured requests when the test ends,
// so a blocked server handler cannot outlive it.
func closeAndDrain[T any](t *testing.T, requests chan T) {
	t.Helper()

	t.Cleanup(func() {
		for {
			select {
			case <-requests:
			default:
				return
			}
		}
	})
}
