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
	"archive/tar"
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/ulikunitz/xz"
)

// TestDownloadURL verifies the download URL assembly, with and without
// credentials.
func TestDownloadURL(t *testing.T) {
	t.Parallel()

	got := downloadURL(testGameVersion, "", "")
	if want := "https://factorio.com/get-download/" + testGameVersion + "/headless/linux64"; got != want {
		t.Errorf("downloadURL() = %q, want %q", got, want)
	}

	got = downloadURL(testGameVersion, "user@example.com", "tok-en")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("could not parse %q: %v", got, err)
	}

	location := parsed.Scheme + "://" + parsed.Host + parsed.Path
	if want := "https://factorio.com/get-download/2.1.24/headless/linux64"; location != want {
		t.Errorf("downloadURL() path = %q, want %q", location, want)
	}

	query := parsed.Query()
	if query.Get("username") != "user@example.com" || query.Get("token") != "tok-en" {
		t.Errorf("downloadURL() query = %v, want username and token", query)
	}
}

// archiveVersion is the game version of the archive fixture.
const archiveVersion = "1.2.3"

// archiveBinaryContent is the content of the fake game binary.
const archiveBinaryContent = "#!/bin/sh\n"

// TestEnsureBinary verifies the archive download and extraction, and
// that an existing binary short-circuits the download.
func TestEnsureBinary(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()

	archiveServer := serveArchive(t)

	binary, err := ensureBinary(t.Context(), logr.Discard(), archiveServer.client,
		cacheDir, archiveVersion, "", "")
	if err != nil {
		t.Fatalf("ensureBinary() error = %v", err)
	}

	if want := gameBinaryPath(cacheDir, archiveVersion); binary != want {
		t.Errorf("ensureBinary() = %q, want %q", binary, want)
	}

	assertExtractedGame(t, cacheDir, binary)

	if got, want := archiveServer.queries(), noQuery; got != want {
		t.Errorf("download queries = %s, want %s", got, want)
	}

	if downloads := archiveServer.downloads(); downloads != 1 {
		t.Errorf("archive downloads = %d, want 1", downloads)
	}

	// A second call must not download again, whatever credentials it carries.
	_, err = ensureBinary(t.Context(), logr.Discard(), archiveServer.client,
		cacheDir, archiveVersion, "user@example.com", "tok-en")
	if err != nil {
		t.Fatalf("ensureBinary() second call error = %v", err)
	}

	if downloads := archiveServer.downloads(); downloads != 1 {
		t.Errorf("archive downloads after the second call = %d, want 1", downloads)
	}
}

// TestEnsureBinaryWithCredentials verifies that the download carries the
// Factorio account credentials as query parameters.
func TestEnsureBinaryWithCredentials(t *testing.T) {
	t.Parallel()

	archiveServer := serveArchive(t)

	_, err := ensureBinary(t.Context(), logr.Discard(), archiveServer.client,
		t.TempDir(), archiveVersion, "user@example.com", "tok-en")
	if err != nil {
		t.Fatalf("ensureBinary() error = %v", err)
	}

	got, want := archiveServer.queries(), "token=tok-en&username=user%40example.com"
	if got != want {
		t.Errorf("download queries = %s, want %s", got, want)
	}
}

// TestEnsureBinaryReportsADownloadFailure verifies that a failing
// download endpoint surfaces as an error and leaves no binary behind.
func TestEnsureBinaryReportsADownloadFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "no such version", http.StatusNotFound)
	}))

	t.Cleanup(server.Close)

	cacheDir := t.TempDir()

	client := &http.Client{
		Timeout:   testHTTPClient().Timeout,
		Transport: newRewriteTransport(t, server.URL),
	}

	binary, err := ensureBinary(t.Context(), logr.Discard(), client, cacheDir, archiveVersion, "", "")
	if err == nil {
		t.Fatalf("ensureBinary() = %q, want an error", binary)
	}

	_, statErr := os.Stat(gameBinaryPath(cacheDir, archiveVersion))
	if !os.IsNotExist(statErr) {
		t.Errorf("a failed download left a game binary behind (stat error = %v)", statErr)
	}

	assertEmptyDir(t, cacheDir)
}

// assertEmptyDir verifies that dir holds no leftover entry.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not read the directory %s: %v", dir, err)
	}

	if len(entries) != 0 {
		t.Errorf("the directory %s holds leftover entries %v", dir, entries)
	}
}

// assertExtractedGame verifies the extracted archive content.
func assertExtractedGame(t *testing.T, cacheDir, binary string) {
	t.Helper()

	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("could not read the extracted binary: %v", err)
	}

	if string(content) != archiveBinaryContent {
		t.Errorf("extracted binary = %q, want the archive content", content)
	}

	_, err = os.Stat(filepath.Join(gameDir(cacheDir, archiveVersion), "data", "core"))
	if err != nil {
		t.Errorf("could not find the extracted data directory: %v", err)
	}

	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("could not stat the extracted binary: %v", err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Errorf("extracted binary mode = %v, want -rwxr-xr-x", info.Mode().Perm())
	}
}

// dirEntry returns a directory archive entry.
func dirEntry() tarEntry {
	return tarEntry{
		mode:    os.ModeDir | 0o755,
		content: "",
		link:    "",
		hard:    "",
	}
}

// fileEntry returns a regular archive entry.
func fileEntry(mode os.FileMode, content string) tarEntry {
	return tarEntry{
		mode:    mode,
		content: content,
		link:    "",
		hard:    "",
	}
}

// hardLinkEntry returns a hard link archive entry.
func hardLinkEntry(target string) tarEntry {
	return tarEntry{
		mode:    0o644,
		content: "",
		link:    "",
		hard:    target,
	}
}

// linkEntry returns a symbolic link archive entry.
func linkEntry(target string) tarEntry {
	return tarEntry{
		mode:    0o777,
		content: "",
		link:    target,
		hard:    "",
	}
}

// downloadEndpoint is a fake factorio.com download endpoint. It serves
// one tar.xz archive of a fake game and records the requests, so the
// tests need neither the real endpoint nor the default HTTP transport.
type downloadEndpoint struct {
	client   *http.Client
	mutex    sync.Mutex
	requests []string
}

// downloads returns how many archives were served.
func (e *downloadEndpoint) downloads() int {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	return len(e.requests)
}

// noQuery marks a request that carried no credentials.
const noQuery = "none"

// queries returns the query strings of the requests in arrival order.
func (e *downloadEndpoint) queries() string {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	rendered := make([]string, 0, len(e.requests))

	for _, query := range e.requests {
		if query == "" {
			rendered = append(rendered, noQuery)

			continue
		}

		rendered = append(rendered, query)
	}

	return strings.Join(rendered, ", ")
}

// buildGameArchive builds the tar.xz archive of a fake game whose binary
// holds content. The extra entries are added next to the game layout.
func buildGameArchive(t *testing.T, content string, extra map[string]tarEntry) []byte {
	t.Helper()

	entries := map[string]tarEntry{
		"factorio/":                 dirEntry(),
		"factorio/bin/":             dirEntry(),
		"factorio/bin/x64/":         dirEntry(),
		"factorio/bin/x64/factorio": fileEntry(0o755, content),
	}

	maps.Copy(entries, extra)

	return buildTarXz(t, entries)
}

// serveArchive starts the fake download endpoint behind a client that
// maps factorio.com URLs to it.
func serveArchive(t *testing.T) *downloadEndpoint {
	t.Helper()

	archive := buildGameArchive(t, archiveBinaryContent, map[string]tarEntry{
		"factorio/data/":     dirEntry(),
		"factorio/data/core": fileEntry(0o644, "core\n"),
	})

	endpoint := &downloadEndpoint{}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/get-download/"+archiveVersion+"/headless/linux64" {
			t.Errorf("unexpected download path %q", request.URL.Path)

			http.NotFound(writer, request)

			return
		}

		endpoint.mutex.Lock()
		endpoint.requests = append(endpoint.requests, request.URL.RawQuery)
		endpoint.mutex.Unlock()

		writer.Header().Set("Content-Type", "application/x-xz")
		_, _ = writer.Write(archive)
	}))

	t.Cleanup(server.Close)

	endpoint.client = &http.Client{
		Timeout:   testHTTPClient().Timeout,
		Transport: newRewriteTransport(t, server.URL),
	}

	return endpoint
}

// rewriteTransport sends factorio.com requests to a test endpoint.
type rewriteTransport struct {
	target  *url.URL
	wrapped http.RoundTripper
}

// newRewriteTransport returns a transport mapping factorio.com URLs to
// the test endpoint.
func newRewriteTransport(t *testing.T, target string) rewriteTransport {
	t.Helper()

	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("could not parse the test endpoint %q: %v", target, err)
	}

	return rewriteTransport{target: parsed, wrapped: http.DefaultTransport}
}

// RoundTrip implements http.RoundTripper.
func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = t.toEndpoint(request)

	return t.wrapped.RoundTrip(request)
}

// toEndpoint returns the request with a factorio.com URL mapped to the
// test endpoint.
func (t rewriteTransport) toEndpoint(request *http.Request) *http.Request {
	if request.URL.Host != "factorio.com" {
		return request
	}

	clone := request.Clone(request.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host

	return clone
}

// TestExtractTarXzRejectsTraversal verifies that archive entries
// escaping the destination are rejected, symlinks included.
func TestExtractTarXzRejectsTraversal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry string
		what  tarEntry
	}{
		{name: "parent traversal", entry: "factorio/../evil", what: fileEntry(0o644, "evil\n")},
		{name: "unexpected layout", entry: "other/file", what: fileEntry(0o644, "evil\n")},
		{name: "absolute entry", entry: "/etc/passwd", what: fileEntry(0o644, "evil\n")},
		{name: "climbing symlink", entry: "factorio/bin/x64/escape", what: linkEntry("../../etc")},
		{name: "absolute symlink", entry: "factorio/bin/x64/escape", what: linkEntry("/etc/passwd")},
		{name: "hard links", entry: "factorio/bin/x64/hard", what: hardLinkEntry("factorio/bin/x64/factorio")},
	}

	for _, test := range tests {
		archive := buildTarXz(t, map[string]tarEntry{test.entry: test.what})

		err := extractTarXz(bytes.NewReader(archive), t.TempDir())
		if err == nil {
			t.Errorf("%s: extractTarXz() error = nil, want an error", test.name)
		} else if !strings.Contains(err.Error(), test.entry) {
			t.Errorf("%s: extractTarXz() error = %v, want it to name %q", test.name, err, test.entry)
		}
	}
}

// TestExtractTarXzSymlink verifies that a symbolic link pointing below
// the destination is created as is.
func TestExtractTarXzSymlink(t *testing.T) {
	t.Parallel()

	archive := buildGameArchive(t, archiveBinaryContent, map[string]tarEntry{
		"factorio/bin/current": linkEntry("x64"),
	})

	dest := t.TempDir()

	err := extractTarXz(bytes.NewReader(archive), dest)
	if err != nil {
		t.Fatalf("extractTarXz() error = %v", err)
	}

	link, err := os.Readlink(filepath.Join(dest, "bin", "current"))
	if err != nil {
		t.Fatalf("could not read the link: %v", err)
	}

	if want := "x64"; link != want {
		t.Errorf("link = %q, want %q", link, want)
	}
}

// tarHeaderFor returns the tar header of a test entry.
func tarHeaderFor(name string, entry tarEntry) *tar.Header {
	header := &tar.Header{
		Name: name,
		Mode: int64(entry.mode.Perm()),
		Size: int64(len(entry.content)),
	}

	switch {
	case entry.mode.IsDir():
		header.Typeflag = tar.TypeDir
	case entry.link != "":
		header.Typeflag = tar.TypeSymlink
		header.Linkname = entry.link
	case entry.hard != "":
		header.Typeflag = tar.TypeLink
		header.Linkname = entry.hard
	case entry.mode.IsRegular() || entry.content != "":
		header.Typeflag = tar.TypeReg
	}

	if header.Typeflag != tar.TypeReg {
		header.Size = 0
	}

	return header
}

// tarEntry is one entry of a test archive. A link or hard link name
// turns the entry into a link.
type tarEntry struct {
	mode    os.FileMode
	content string
	link    string
	hard    string
}

// buildTarXz builds an in-memory tar.xz archive of the entries.
func buildTarXz(t *testing.T, entries map[string]tarEntry) []byte {
	t.Helper()

	var buffer bytes.Buffer

	xzWriter, err := xz.NewWriter(&buffer)
	if err != nil {
		t.Fatalf("could not create the xz writer: %v", err)
	}

	tarWriter := tar.NewWriter(xzWriter)

	for name, entry := range entries {
		err := tarWriter.WriteHeader(tarHeaderFor(name, entry))
		if err != nil {
			t.Fatalf("could not write the tar header of %s: %v", name, err)
		}

		if entry.content != "" {
			_, err := tarWriter.Write([]byte(entry.content))
			if err != nil {
				t.Fatalf("could not write the tar entry %s: %v", name, err)
			}
		}
	}

	err = tarWriter.Close()
	if err != nil {
		t.Fatalf("could not close the tar writer: %v", err)
	}

	err = xzWriter.Close()
	if err != nil {
		t.Fatalf("could not close the xz writer: %v", err)
	}

	return buffer.Bytes()
}
