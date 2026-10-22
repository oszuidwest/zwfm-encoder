package recording

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const recordingTimestampLayout = "2006-01-02-15-04"

// recordingNameTail matches what follows the ownership prefix in a recording
// filename: the start minute, an optional collision suffix and the extension of
// a current (mp3, ts) or removed (ogg, mkv, mp2) codec. Keep it in sync with
// types.codecPresets, or files of a new codec never expire.
var recordingNameTail = regexp.MustCompile(`^-(\d{4}-\d{2}-\d{2}-\d{2}-\d{2})(?:-\d+)?\.(?:mp3|ts|ogg|mkv|mp2)$`)

func (m *Manager) runCleanup() {
	if m.outbox.unreadable > 0 {
		// An unreadable ticket may own any old file: expire nothing.
		slog.Warn("cleanup: skipped, the upload queue holds tickets that could not be read", "count", m.outbox.unreadable)
		return
	}
	m.mu.RLock()
	recorders := slices.Collect(maps.Values(m.recorders))
	m.mu.RUnlock()

	slog.Info("cleanup: starting", "recorders", len(recorders))

	for _, recorder := range recorders {
		cfg := recorder.Config()

		// Skip if retention is 0 (keep forever)
		if cfg.RetentionDays == 0 {
			continue
		}

		if cfg.StorageMode == types.StorageLocal || cfg.StorageMode == types.StorageBoth {
			m.cleanupLocalFiles(recorder)
		}

		if cfg.StorageMode == types.StorageS3 || cfg.StorageMode == types.StorageBoth {
			m.cleanupS3Files(recorder)
		}
	}

	slog.Info("cleanup: completed")
}

// cleanupLocalFiles removes local files older than retention days.
func (m *Manager) cleanupLocalFiles(recorder *GenericRecorder) {
	cfg := recorder.Config()
	if cfg.LocalPath == "" {
		return
	}

	cutoff := util.OldestToKeep(cfg.RetentionDays, time.Now())

	entries, err := os.ReadDir(cfg.LocalPath)
	if err != nil {
		slog.Warn("cleanup: failed to read local directory", "id", cfg.ID, "path", cfg.LocalPath, "error", err)
		return
	}

	var deleted int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Only process files matching this recorder's pattern.
		fileDate, ok := recorderFileTime(&cfg, name)
		if !ok {
			continue
		}

		if fileDate.Before(cutoff) {
			filePath := filepath.Join(cfg.LocalPath, name)

			// A recording that is being written, finalized or not yet
			// delivered, or that failed, is never expired.
			if m.outbox.hasPath(filePath) {
				continue
			}

			// A file still written to within retention keeps recent audio, even
			// when its name is old (a long recording, or a clock fixed after boot).
			// Lstat reads fresh metadata; a DirEntry may cache it on Windows.
			if info, err := os.Lstat(filePath); err != nil || !info.ModTime().Before(cutoff) {
				continue
			}

			if err := os.Remove(filePath); err != nil {
				slog.Warn("cleanup: failed to delete local file", "id", cfg.ID, "path", filePath, "error", err)
			} else {
				deleted++
				slog.Debug("cleanup: deleted local file", "id", cfg.ID, "file", name)
			}
		}
	}

	if deleted > 0 {
		slog.Info("cleanup: deleted local files", "id", cfg.ID, "count", deleted)
		m.logCleanupEvent(cfg.Name, deleted, "local")
	}
}

// cleanupS3Files removes S3 objects older than retention days using pagination.
func (m *Manager) cleanupS3Files(recorder *GenericRecorder) {
	cfg := recorder.Config()
	if cfg.S3Bucket == "" {
		return
	}

	if !cfg.HasS3Credentials() {
		slog.Warn("cleanup: no S3 client available", "id", cfg.ID)
		return
	}

	client := createS3Client(RecorderToS3Config(&cfg))
	cutoff := util.OldestToKeep(cfg.RetentionDays, time.Now())

	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		5*time.Minute,
		errors.New("s3 cleanup timeout"),
	)
	defer cancel()

	var deleted int
	for _, filePrefix := range ownedFilePrefixes(&cfg) {
		n, err := cleanupS3Namespace(ctx, client, &cfg, s3Namespace(filePrefix), cutoff)
		deleted += n
		if err != nil {
			slog.Warn("cleanup: failed to list S3 objects", "id", cfg.ID, "bucket", cfg.S3Bucket, "prefix", s3Namespace(filePrefix), "error", err)
		}
	}
	if deleted > 0 {
		slog.Info("cleanup: deleted S3 objects", "id", cfg.ID, "count", deleted)
		m.logCleanupEvent(cfg.Name, deleted, "s3")
	}
}

// cleanupS3Namespace deletes expired objects directly under prefix and returns how many it deleted.
func cleanupS3Namespace(ctx context.Context, client *s3.Client, cfg *types.Recorder, prefix string, cutoff time.Time) (int, error) {
	var deleted int
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(cfg.S3Bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, err
		}
		for _, obj := range output.Contents {
			key := aws.ToString(obj.Key)
			filename, ok := strings.CutPrefix(key, prefix)
			if !ok || strings.Contains(filename, "/") {
				continue // Never delete outside the recorder's exact namespace.
			}
			// Spool recovery can upload legacy-named files into the ID namespace, so match only the timestamp.
			fileDate, ok := util.FilenameTime(filename)
			// Delete if older than retention, by name and by upload time (a
			// missing LastModified falls back to the name alone).
			if !ok || !fileDate.Before(cutoff) || !aws.ToTime(obj.LastModified).Before(cutoff) {
				continue
			}
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(cfg.S3Bucket), Key: obj.Key}); err != nil {
				slog.Warn("cleanup: failed to delete S3 object", "id", cfg.ID, "key", key, "error", err)
				continue
			}
			deleted++
			slog.Debug("cleanup: deleted S3 object", "id", cfg.ID, "key", key)
		}
	}
	return deleted, nil
}

// ownedFilePrefixes lists the ID namespace and, for pre-upgrade recorders, the captured legacy prefix.
func ownedFilePrefixes(cfg *types.Recorder) []string {
	if cfg.LegacyFilePrefix == "" {
		return []string{cfg.RecordingFilePrefix()}
	}
	return []string{cfg.RecordingFilePrefix(), cfg.LegacyFilePrefix}
}

func recorderFileTime(cfg *types.Recorder, filename string) (time.Time, bool) {
	for _, prefix := range ownedFilePrefixes(cfg) {
		if at, ok := recordingFileTime(prefix, filename); ok {
			return at, true
		}
	}
	return time.Time{}, false
}

// recordingFileTime parses "<safeName>-YYYY-MM-DD-HH-MM[-N].<ext>" only when
// safeName matches the complete ownership prefix (ID-based or legacy).
func recordingFileTime(safeName, filename string) (time.Time, bool) {
	tail, ok := strings.CutPrefix(filename, safeName)
	if !ok {
		return time.Time{}, false
	}
	match := recordingNameTail.FindStringSubmatch(tail)
	if match == nil {
		return time.Time{}, false
	}
	fileTime, err := time.ParseInLocation(recordingTimestampLayout, match[1], time.Local)
	return fileTime, err == nil
}

func (m *Manager) logCleanupEvent(recorderName string, filesDeleted int, storageType string) {
	if m.eventLogger == nil {
		return
	}
	if err := m.eventLogger.LogRecorder(eventlog.CleanupCompleted, &eventlog.RecorderDetails{
		RecorderName: recorderName,
		FilesDeleted: filesDeleted,
		StorageType:  storageType,
	}); err != nil {
		slog.Warn("failed to log cleanup event", "error", err)
	}
}
