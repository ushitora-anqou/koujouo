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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Discord webhook posting parameters. The retry follows design.md
// section 1.5: three attempts with exponential backoff on 429 and 5xx.
const (
	notifyAttempts       = 3
	notifyBackoffBase    = 1 * time.Second
	notifyUsername       = "Factorio"
	notifyRequestTimeout = 10 * time.Second
)

// notifier posts messages to a Discord webhook. A nil notifier or an
// empty webhook URL disables notifications.
type notifier struct {
	client     *http.Client
	webhookURL string
	backoff    time.Duration
}

// newNotifier returns a notifier for the webhook URL, or nil when the
// URL is empty.
func newNotifier(webhookURL string, client *http.Client) *notifier {
	if webhookURL == "" {
		return nil
	}

	return &notifier{
		client:     client,
		webhookURL: webhookURL,
		backoff:    notifyBackoffBase,
	}
}

// webhookBody is the JSON body of a Discord webhook post.
type webhookBody struct {
	Content  string `json:"content"`
	Username string `json:"username"`
}

// Send posts content to the webhook, retrying on 429 and 5xx.
func (n *notifier) Send(ctx context.Context, content string) error {
	if n == nil {
		return nil
	}

	encoded, err := json.Marshal(webhookBody{Content: content, Username: notifyUsername})
	if err != nil {
		return fmt.Errorf("could not encode the notification body: %w", err)
	}

	var lastErr error

	for attempt := range notifyAttempts {
		if attempt > 0 {
			err := sleepContext(ctx, n.backoff*time.Duration(1<<(attempt-1)))
			if err != nil {
				return err
			}
		}

		lastErr = n.post(ctx, encoded)
		if lastErr == nil {
			return nil
		}

		if _, ok := errors.AsType[*webhookClientError](lastErr); ok {
			return lastErr
		}
	}

	return lastErr
}

// post sends one webhook request and reports whether it failed with a
// retryable status.
func (n *notifier) post(ctx context.Context, encoded []byte) error {
	ctx, cancel := context.WithTimeout(ctx, notifyRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("could not build the webhook request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := n.client.Do(request)
	if err != nil {
		return fmt.Errorf("could not post the webhook message: %w", err)
	}

	defer response.Body.Close() //nolint:errcheck // The body of a finished webhook response needs no drain.

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("webhook returned %d: %w", response.StatusCode, errWebhookRetryable)
	}

	// Client errors other than 429 never succeed on retry; fail hard
	// wrapped so Send stops retrying.
	return &webhookClientError{status: response.StatusCode}
}

// errWebhookRetryable marks a webhook response worth retrying.
var errWebhookRetryable = errors.New("the webhook response is retryable")

// webhookClientError marks a webhook response that must not be retried.
type webhookClientError struct {
	status int
}

// Error implements the error interface.
func (e *webhookClientError) Error() string {
	return fmt.Sprintf("webhook returned the client error status %d: %s", e.status, errWebhookClient)
}

// errWebhookClient marks a webhook response that fails the delivery.
var errWebhookClient = errors.New("the webhook rejected the message")

// sleepContext sleeps for duration, or returns early when ctx is done.
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("could not wait before the retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
