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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// Fixtures of the backup tests.
const (
	// backupBucket and the keys are the static credentials of the fake
	// S3 endpoint.
	backupBucket     = "example-bucket"
	backupAccessKey  = "test-access-key"
	backupSecretKey  = "test-secret-key"
	testSaveContent  = "save-bytes"
	oldSaveContent   = "an older world"
	finishedSaveName = "_autosave1.zip"

	// temporarySaveName is an unfinished save, never uploaded.
	temporarySaveName = "_autosave2.tmp.zip"

	// notASaveName is a file next to the saves, never uploaded.
	notASaveName = "map.zip.ready.39"

	// backupWaitDeadline bounds the wait for the background uploads.
	backupWaitDeadline = 5 * time.Second

	// uploadCountAtStart and uploadCountAfterStop are the uploads of a
	// game life cycle.
	uploadCountAtStart   = 1
	uploadCountAfterStop = 2
	autoSaveAge          = 2 * time.Hour
	worldSaveAge         = time.Hour
	futureSaveAge        = time.Minute
	sigV4Prefix          = "AWS4-HMAC-SHA256"
)

// testBackupMoment returns the fixed upload moment of the object key
// tests.
func testBackupMoment() time.Time {
	return time.Date(2026, time.November, 23, 13, 10, 0, 0, time.UTC)
}

// TestBackupKey verifies the object key layout of a save upload.
func TestBackupKey(t *testing.T) {
	t.Parallel()

	got := backupKey(testBackupMoment())
	if want := "factorio-map-20261123-131000.zip"; got != want {
		t.Errorf("backupKey() = %q, want %q", got, want)
	}
}

// TestRegionFromEndpointURL verifies the SigV4 region derivation.
func TestRegionFromEndpointURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		endpointURL string
		want        string
	}{
		{endpointURL: "https://s3.us-west-00.backblazeb2.com", want: "us-west-00"},
		{endpointURL: "https://s3.eu-central-00.backblazeb2.com", want: "eu-central-00"},
		{endpointURL: "http://127.0.0.1:9000", want: backupRegionFallback},
		{endpointURL: "https://example.com", want: backupRegionFallback},
	}

	for _, test := range tests {
		if got := regionFromEndpointURL(test.endpointURL); got != test.want {
			t.Errorf("regionFromEndpointURL(%q) = %q, want %q", test.endpointURL, got, test.want)
		}
	}
}

// capturedUpload is one S3 PutObject request.
type capturedUpload struct {
	method        string
	path          string
	authorization string
	body          string
}

// uploadCapture records the requests of the fake S3 endpoint.
type uploadCapture struct {
	mutex    sync.Mutex
	requests []capturedUpload
}

// record stores one captured request.
func (c *uploadCapture) record(request capturedUpload) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.requests = append(c.requests, request)
}

// waitForCount returns the captured requests once count arrived.
func (c *uploadCapture) waitForCount(t *testing.T, count int) []capturedUpload {
	t.Helper()

	deadline := time.Now().Add(backupWaitDeadline)

	for {
		c.mutex.Lock()
		requests := append([]capturedUpload{}, c.requests...)
		c.mutex.Unlock()

		if len(requests) >= count {
			return requests
		}

		if time.Now().After(deadline) {
			t.Fatalf("uploads = %d, want at least %d", len(requests), count)

			return nil
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// newUploadCaptureServer serves an S3 endpoint that records its
// requests.
func newUploadCaptureServer(t *testing.T) (*httptest.Server, *uploadCapture) {
	t.Helper()

	capture := &uploadCapture{}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		content, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("could not read the upload body: %v", err)
		}

		capture.record(capturedUpload{
			method:        request.Method,
			path:          request.URL.Path,
			authorization: request.Header.Get("Authorization"),
			body:          string(content),
		})

		writer.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(server.Close)

	return server, capture
}

// backupSettings returns the backup settings pointing at the fake S3
// endpoint.
func backupSettings(endpointURL string) *BackupSettings {
	return &BackupSettings{
		EndpointURL:     endpointURL,
		Bucket:          backupBucket,
		AccessKeyID:     backupAccessKey,
		SecretAccessKey: backupSecretKey,
	}
}

// assertUploadRequest verifies one captured PutObject request.
func assertUploadRequest(t *testing.T, captured capturedUpload) {
	t.Helper()

	if captured.method != http.MethodPut {
		t.Errorf("upload method = %q, want %q", captured.method, http.MethodPut)
	}

	const pathPrefix = "/" + backupBucket + "/" + backupKeyPrefix

	if !strings.HasPrefix(captured.path, pathPrefix) || !strings.HasSuffix(captured.path, ".zip") {
		t.Errorf("upload path = %q, want a path-style key below %s", captured.path, pathPrefix)
	}

	if !strings.HasPrefix(captured.authorization, sigV4Prefix) {
		t.Errorf("upload authorization = %q, want a SigV4 header", captured.authorization)
	}
}

// TestUploaderUpload verifies the S3 PutObject request: path-style
// addressing, the bucket and key in the path, the SigV4 authorization
// header and the save file as the body.
func TestUploaderUpload(t *testing.T) {
	t.Parallel()

	server, capture := newUploadCaptureServer(t)

	savePath := writeSaveFile(t, t.TempDir(), saveFileName, testSaveContent, time.Now())

	upload, err := newUploader(t.Context(), backupSettings(server.URL))
	if err != nil {
		t.Fatalf("newUploader() error = %v", err)
	}

	key := backupKey(testBackupMoment())

	err = upload.upload(t.Context(), logr.Discard(), savePath, key)
	if err != nil {
		t.Fatalf("upload() error = %v", err)
	}

	captured := capture.waitForCount(t, 1)[0]

	assertUploadRequest(t, captured)

	if want := "/" + backupBucket + "/" + key; captured.path != want {
		t.Errorf("upload path = %q, want %q", captured.path, want)
	}

	if captured.body != testSaveContent {
		t.Errorf("upload body = %q, want %q", captured.body, testSaveContent)
	}
}

// writeSaveFile writes a file below dir with the given modification
// time and returns its path.
func writeSaveFile(t *testing.T, dir, name, content string, modified time.Time) string {
	t.Helper()

	err := os.MkdirAll(dir, directoryFileMode)
	if err != nil {
		t.Fatalf("could not create the directory %s: %v", dir, err)
	}

	path := filepath.Join(dir, name)

	err = os.WriteFile(path, []byte(content), privateFileMode)
	if err != nil {
		t.Fatalf("could not write %s: %v", path, err)
	}

	err = os.Chtimes(path, modified, modified)
	if err != nil {
		t.Fatalf("could not set the modification time of %s: %v", path, err)
	}

	return path
}

// TestLatestSave verifies that the newest finished save file is picked:
// the autosaves sit next to the save the server was started with, and
// an unfinished temporary save is never taken.
func TestLatestSave(t *testing.T) {
	t.Parallel()

	savesDir := t.TempDir()

	_, err := latestSave(savesDir)
	if !errors.Is(err, errNoSave) {
		t.Errorf("latestSave(empty directory) error = %v, want %v", err, errNoSave)
	}

	now := time.Now()

	writeSaveFile(t, savesDir, finishedSaveName, oldSaveContent, now.Add(-autoSaveAge))
	writeSaveFile(t, savesDir, saveFileName, testSaveContent, now.Add(-worldSaveAge))
	writeSaveFile(t, savesDir, temporarySaveName, "half written", now.Add(futureSaveAge))
	writeSaveFile(t, savesDir, notASaveName, "not a save", now.Add(futureSaveAge))

	mkdirErr := os.Mkdir(filepath.Join(savesDir, "factorio-save.zip"), directoryFileMode)
	if mkdirErr != nil {
		t.Fatalf("could not create the directory entry: %v", mkdirErr)
	}

	path, err := latestSave(savesDir)
	if err != nil {
		t.Fatalf("latestSave() error = %v", err)
	}

	if want := filepath.Join(savesDir, saveFileName); path != want {
		t.Errorf("latestSave() = %q, want %q", path, want)
	}

	// A newer autosave becomes the latest world state.
	writeSaveFile(t, savesDir, finishedSaveName, oldSaveContent, now.Add(futureSaveAge))

	path, err = latestSave(savesDir)
	if err != nil {
		t.Fatalf("latestSave() error = %v", err)
	}

	if want := filepath.Join(savesDir, finishedSaveName); path != want {
		t.Errorf("latestSave() with a newer autosave = %q, want %q", path, want)
	}
}

// TestAgentBacksUpTheNewestSave verifies the upload schedule of the
// design: one upload when the game starts and one after the graceful
// stop, both carrying the newest save file.
func TestAgentBacksUpTheNewestSave(t *testing.T) {
	t.Parallel()

	server, capture := newUploadCaptureServer(t)

	saveDir := t.TempDir()

	cacheDir := t.TempDir()

	plantFakeFactorio(t, cacheDir, testGameVersion)

	now := time.Now()

	writeSaveFile(t, filepath.Join(saveDir, "saves"), finishedSaveName, oldSaveContent, now.Add(-autoSaveAge))
	writeSaveFile(t, filepath.Join(saveDir, "saves"), saveFileName, testSaveContent, now.Add(-worldSaveAge))

	config := roundTripConfig()
	config.Backup = backupSettings(server.URL)

	client, gameAgent := newRPCAgent(t, logr.Discard(), roundTripOptions(saveDir, cacheDir, nil))

	t.Cleanup(func() { killGame(gameAgent) })

	callConfigure(t, client, config)

	waitForReady(t, gameAgent)

	// The upload that starts with the game.
	assertUploadRequest(t, capture.waitForCount(t, uploadCountAtStart)[0])

	callStop(t, client)

	// The upload after the graceful stop.
	uploads := capture.waitForCount(t, uploadCountAfterStop)

	for _, upload := range uploads {
		assertUploadRequest(t, upload)

		if upload.body != testSaveContent {
			t.Errorf("upload body = %q, want the newest save %q", upload.body, testSaveContent)
		}
	}
}
