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
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-logr/logr"
)

// Backup parameters. Uploads run on an hourly ticker and once after
// every graceful stop; old objects are never deleted.
const (
	backupKeyPrefix      = "factorio-map-"
	backupKeyTimeFormat  = "20060102-150405"
	defaultBackupPeriod  = 1 * time.Hour
	backupRegionFallback = "us-west-00"
	saveFileSuffix       = ".zip"
	tempSaveSuffix       = ".tmp.zip"

	// backupTimeout bounds one upload. A save is tens of megabytes and
	// can exceed 100 MB, which a minute does not cover on a slow S3
	// compatible store; a failed upload waits for the next period.
	backupTimeout = 5 * time.Minute
)

// errNoSave marks a saves directory without a finished save file.
var errNoSave = errors.New("no finished save file")

// uploader uploads the save file to an S3-compatible bucket. Only
// PutObject is used; the object key layout makes old objects stay.
type uploader struct {
	client *s3.Client
	bucket string
}

// newUploader builds the S3 client for the backup settings. Path-style
// addressing and the static credentials cover Backblaze B2 and other
// S3-compatible stores.
func newUploader(ctx context.Context, settings *BackupSettings) (*uploader, error) {
	config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(settings.AccessKeyID, settings.SecretAccessKey, ""),
		),
		awsconfig.WithRegion(regionFromEndpointURL(settings.EndpointURL)),
	)
	if err != nil {
		return nil, fmt.Errorf("could not load the S3 client configuration: %w", err)
	}

	client := s3.NewFromConfig(config, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(settings.EndpointURL)
		options.UsePathStyle = true
	})

	return &uploader{client: client, bucket: settings.Bucket}, nil
}

// upload stores path under key.
func (u *uploader) upload(ctx context.Context, log logr.Logger, path, key string) error {
	file, err := os.Open(path) //nolint:gosec // The path is built by the agent itself.
	if err != nil {
		return fmt.Errorf("could not open the save file %s: %w", path, err)
	}

	defer file.Close() //nolint:errcheck // Read-only, nothing to flush.

	input := &s3.PutObjectInput{ //nolint:exhaustruct_v5 // Only the object destination and body matter.
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   file,
	}

	_, err = u.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("could not upload %s to %s: %w", key, u.bucket, err)
	}

	log.Info("Uploaded the save file", "path", path, "bucket", u.bucket, "key", key)

	return nil
}

// backupKey returns the object key of a save upload at t, for example
// factorio-map-20261123-131000.zip.
func backupKey(t time.Time) string {
	return backupKeyPrefix + t.UTC().Format(backupKeyTimeFormat) + ".zip"
}

// regionFromEndpointURL derives the SigV4 region from an S3 endpoint
// host such as s3.us-west-00.backblazeb2.com. S3-compatible stores
// accept any region, so unparseable hosts fall back to a placeholder.
func regionFromEndpointURL(endpointURL string) string {
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return backupRegionFallback
	}

	host := parsed.Hostname()
	labels := strings.Split(host, ".")

	if len(labels) >= 3 && labels[0] == "s3" {
		return labels[1]
	}

	return backupRegionFallback
}

// backupLoop uploads the save file once and then every backup period,
// until the context ends.
func (a *Agent) backupLoop(ctx context.Context) {
	a.backupOnce(ctx)

	ticker := time.NewTicker(a.backupPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.backupOnce(ctx)
		}
	}
}

// backupOnce uploads the newest save file one time. It does nothing
// while backups are disabled.
func (a *Agent) backupOnce(ctx context.Context) {
	upload := a.currentUploader()
	if upload == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()

	path, err := latestSave(a.savesDir())
	if err != nil {
		a.log.Error(err, "Could not find the save file for the backup")

		return
	}

	err = upload.upload(ctx, a.log, path, backupKey(a.now()))
	if err != nil {
		a.log.Error(err, "Could not upload the save file")
	}
}

// latestSave returns the newest finished save file of the saves
// directory. Factorio rotates its autosaves next to the save the server
// was started with, so the newest finished file holds the latest world
// state; an unfinished temporary save is never uploaded.
func latestSave(savesDir string) (string, error) {
	entries, err := os.ReadDir(savesDir)
	if err != nil {
		return "", fmt.Errorf("could not list the saves directory %s: %w", savesDir, err)
	}

	newest := ""

	var newestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() || !isFinishedSave(entry.Name()) {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return "", fmt.Errorf("could not inspect the save file %s: %w", entry.Name(), infoErr)
		}

		if newest == "" || info.ModTime().After(newestTime) {
			newest = filepath.Join(savesDir, entry.Name())
			newestTime = info.ModTime()
		}
	}

	if newest == "" {
		return "", fmt.Errorf("could not find a save file in %s: %w", savesDir, errNoSave)
	}

	return newest, nil
}

// isFinishedSave reports whether name is a complete save file.
func isFinishedSave(name string) bool {
	return strings.HasSuffix(name, saveFileSuffix) && !strings.HasSuffix(name, tempSaveSuffix)
}
