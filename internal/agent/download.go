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
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	"github.com/ulikunitz/xz"
)

// Download layout under the cache directory. The archive of one version
// is extracted to cacheDir/factorio-<version> with the leading
// "factorio/" component of the entries stripped, so the game binary
// sits at factorio-<version>/bin/x64/factorio.
const (
	downloadBaseURL   = "https://factorio.com/get-download/"
	downloadUserAgent = "koujouo-agent/0.0.1"
	tarRootComponent  = "factorio"
	binaryRelativeDir = "bin/x64/factorio"
	archiveName       = "factorio.tar.xz"
)

// downloadURL builds the URL of the headless linux64 build of version.
// Username and token are attached as query parameters when set, which
// experimental and older builds require.
func downloadURL(version, username, token string) string {
	endpoint := downloadBaseURL + url.PathEscape(version) + "/headless/linux64"

	if username == "" && token == "" {
		return endpoint
	}

	query := url.Values{}
	query.Set("username", username)
	query.Set("token", token)

	return endpoint + "?" + query.Encode()
}

// gameBinaryPath returns the expected path of the game binary of
// version under cacheDir.
func gameBinaryPath(cacheDir, version string) string {
	return filepath.Join(gameDir(cacheDir, version), filepath.FromSlash(binaryRelativeDir))
}

// gameDir returns the extraction directory of version under cacheDir.
func gameDir(cacheDir, version string) string {
	return filepath.Join(cacheDir, "factorio-"+version)
}

// ensureBinary returns the path of the game binary of version,
// downloading and extracting the archive when it is not on the ephemeral
// volume yet. It is idempotent: an existing binary short-circuits the
// download.
func ensureBinary(
	ctx context.Context,
	log logr.Logger,
	client *http.Client,
	cacheDir, version, username, token string,
) (string, error) {
	binary := gameBinaryPath(cacheDir, version)

	_, statErr := os.Stat(binary)
	if statErr == nil {
		return binary, nil
	}

	if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("could not stat the game binary %s: %w", binary, statErr)
	}

	err := fetchAndExtract(ctx, log, client, cacheDir, version, username, token)
	if err != nil {
		return "", err
	}

	log.Info("Extracted the game archive", "version", version, "binary", binary)

	return binary, nil
}

// fetchAndExtract downloads the archive of version into a temporary
// directory, extracts it and moves it to its final directory.
func fetchAndExtract(
	ctx context.Context,
	log logr.Logger,
	client *http.Client,
	cacheDir, version, username, token string,
) error {
	temporary := filepath.Join(cacheDir, ".download-"+version+".tmp")

	err := os.RemoveAll(temporary)
	if err != nil {
		return fmt.Errorf("could not clean up the download directory %s: %w", temporary, err)
	}

	err = os.MkdirAll(temporary, directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the download directory %s: %w", temporary, err)
	}

	defer func() {
		err := os.RemoveAll(temporary)
		if err != nil {
			log.Error(err, "Could not remove the download directory", "directory", temporary)
		}
	}()

	archive := filepath.Join(temporary, archiveName)

	err = downloadArchive(ctx, client, downloadURL(version, username, token), archive)
	if err != nil {
		return err
	}

	return installArchive(archive, gameDir(cacheDir, version))
}

// installArchive extracts the tar.xz archive and moves the content to
// dir.
func installArchive(archive, dir string) error {
	extracted := filepath.Join(filepath.Dir(archive), "extracted")

	err := os.MkdirAll(extracted, directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the extraction directory %s: %w", extracted, err)
	}

	err = extractTarXzFile(archive, extracted)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(dir), directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the game directory parent of %s: %w", dir, err)
	}

	// The binary is only complete once the directory is in place, so a
	// directory that exists here holds no usable game.
	err = os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("could not remove the incomplete game directory %s: %w", dir, err)
	}

	err = os.Rename(extracted, dir)
	if err != nil {
		return fmt.Errorf("could not move the extracted game to %s: %w", dir, err)
	}

	return nil
}

// downloadArchive downloads the archive at endpoint to path.
func downloadArchive(ctx context.Context, client *http.Client, endpoint, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("could not build the download request for %s: %w", endpoint, err)
	}

	request.Header.Set("User-Agent", downloadUserAgent)

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("could not download %s: %w", endpoint, err)
	}

	defer response.Body.Close() //nolint:errcheck // The body is fully read or the download failed.

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("could not download %s: unexpected status %d: %w",
			endpoint, response.StatusCode, errDownloadStatus)
	}

	file, err := os.Create(path) //nolint:gosec // The path is built by the agent itself.
	if err != nil {
		return fmt.Errorf("could not create the archive file %s: %w", path, err)
	}

	_, err = io.Copy(file, response.Body)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("could not write the archive file %s: %w", path, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("could not close the archive file %s: %w", path, err)
	}

	return nil
}

// errTarEntry marks an unsupported or unsafe tar entry.
var errTarEntry = errors.New("unsupported tar entry")

// errDownloadStatus marks an unexpected download status.
var errDownloadStatus = errors.New("the download endpoint answered an unexpected status")

// extractTarXzFile extracts the tar.xz archive at path into dest.
func extractTarXzFile(path, dest string) error {
	file, err := os.Open(path) //nolint:gosec // The path is built by the agent itself.
	if err != nil {
		return fmt.Errorf("could not open the archive %s: %w", path, err)
	}

	defer file.Close() //nolint:errcheck // Read-only, nothing to flush.

	return extractTarXz(file, dest)
}

// extractTarXz extracts a tar.xz stream into dest. The entries are
// expected below a single leading component, which is stripped. Entries
// escaping dest are rejected.
func extractTarXz(reader io.Reader, dest string) error {
	xzReader, err := xz.NewReader(reader)
	if err != nil {
		return fmt.Errorf("could not decompress the archive: %w", err)
	}

	tarReader := tar.NewReader(xzReader)

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("could not read the archive: %w", err)
		}

		err = extractTarEntry(tarReader, header, dest)
		if err != nil {
			return err
		}
	}
}

// extractTarEntry writes one tar entry below dest.
func extractTarEntry(tarReader *tar.Reader, header *tar.Header, dest string) error {
	target, err := tarTarget(header.Name, dest)
	if err != nil {
		return err
	}

	if header.Typeflag == tar.TypeDir {
		err = os.MkdirAll(target, header.FileInfo().Mode())
		if err != nil {
			return fmt.Errorf("could not create the directory %s: %w", target, err)
		}

		return nil
	}

	if header.Typeflag == tar.TypeReg {
		return extractTarFile(tarReader, target, header.FileInfo().Mode())
	}

	if header.Typeflag == tar.TypeSymlink {
		return extractTarSymlink(header, target, dest)
	}

	return fmt.Errorf("could not extract %s: unsupported entry type %q: %w",
		header.Name, string(header.Typeflag), errTarEntry)
}

// extractTarSymlink creates a symbolic link entry, provided the link
// points below dest.
func extractTarSymlink(header *tar.Header, target, dest string) error {
	if !symlinkStaysBelow(header.Linkname, target, dest) {
		return fmt.Errorf("could not extract %s: the link to %q escapes the destination: %w",
			header.Name, header.Linkname, errTarEntry)
	}

	err := os.MkdirAll(filepath.Dir(target), directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the parent directory of %s: %w", target, err)
	}

	err = os.Symlink(header.Linkname, target)
	if err != nil {
		return fmt.Errorf("could not create the link %s: %w", target, err)
	}

	return nil
}

// symlinkStaysBelow reports whether the link of an archive entry,
// created at target, points below dest. Absolute links and links that
// climb with ".." are refused: a climb could run through a symlink of
// the archive itself and leave dest behind the check.
func symlinkStaysBelow(linkname, target, dest string) bool {
	if filepath.IsAbs(linkname) || slices.Contains(strings.Split(filepath.ToSlash(linkname), "/"), "..") {
		return false
	}

	resolved := filepath.Join(filepath.Dir(target), linkname)

	return resolved == dest || strings.HasPrefix(resolved, dest+string(os.PathSeparator))
}

// extractTarFile writes one regular tar entry to target.
func extractTarFile(tarReader *tar.Reader, target string, mode os.FileMode) error {
	err := os.MkdirAll(filepath.Dir(target), directoryFileMode)
	if err != nil {
		return fmt.Errorf("could not create the parent directory of %s: %w", target, err)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC

	file, err := os.OpenFile(target, flags, mode) //nolint:gosec // The target stays below dest.
	if err != nil {
		return fmt.Errorf("could not create the file %s: %w", target, err)
	}

	_, err = io.Copy(file, tarReader)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("could not write the file %s: %w", target, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("could not close the file %s: %w", target, err)
	}

	return nil
}

// tarTarget maps one archive entry name to its path under dest. The
// leading component of the entry name is stripped, absolute names and
// names escaping dest are rejected.
func tarTarget(name, dest string) (string, error) {
	stripped, found := strings.CutPrefix(name, tarRootComponent+"/")
	if !found && name == tarRootComponent {
		stripped = ""
	} else if !found {
		return "", fmt.Errorf("could not extract %s: unexpected archive layout: %w", name, errTarEntry)
	}

	cleaned := filepath.Clean(stripped)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("could not extract %s: path escapes the destination: %w", name, errTarEntry)
	}

	target := filepath.Join(dest, cleaned)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("could not extract %s: path escapes the destination: %w", name, errTarEntry)
	}

	return target, nil
}
