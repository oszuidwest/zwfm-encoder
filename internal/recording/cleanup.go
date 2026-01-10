package recording

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// startCleanupScheduler starts the hourly cleanup scheduler.
func (m *Manager) startCleanupScheduler() {
	go func() {
		for {
			duration := util.TimeUntilNextHour(time.Now())

			slog.Info("cleanup scheduler: next run scheduled", "in", duration.Round(time.Second))

			select {
			case <-time.After(duration):
				m.runCleanup()
			case <-m.cleanupStopCh:
				slog.Info("cleanup scheduler stopped")
				return
			}
		}
	}()
}

// runCleanup performs cleanup for all recorders.
func (m *Manager) runCleanup() {
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

		// Cleanup local files if applicable
		if cfg.StorageMode == types.StorageLocal || cfg.StorageMode == types.StorageBoth {
			m.cleanupLocalFiles(recorder)
		}

		// Cleanup S3 files if applicable
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

	cutoff := util.RetentionCutoff(cfg.RetentionDays, time.Now())
	safeName := sanitizeFilename(cfg.Name)

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

		// Only process files matching this recorder's pattern
		if !strings.HasPrefix(name, safeName+"-") {
			continue
		}

		// Extract date from filename
		fileDate, ok := util.ExtractDateFromFilename(name)
		if !ok {
			continue
		}

		// Delete if older than retention
		if fileDate.Before(cutoff) {
			filePath := filepath.Join(cfg.LocalPath, name)

			// Skip if file is currently being recorded
			if recorder.IsCurrentFile(filePath) {
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

// cleanupS3Files removes S3 objects older than retention days.
func (m *Manager) cleanupS3Files(recorder *GenericRecorder) {
	cfg := recorder.Config()
	if cfg.S3Bucket == "" {
		return
	}

	// Get S3 client (recreates if config changed - same pattern as Graph client)
	client, err := recorder.getOrCreateS3Client()
	if err != nil {
		slog.Error("cleanup: failed to create S3 client", "id", cfg.ID, "error", err)
		return
	}
	if client == nil {
		slog.Warn("cleanup: no S3 client available", "id", cfg.ID)
		return
	}

	cutoff := util.RetentionCutoff(cfg.RetentionDays, time.Now())
	safeName := sanitizeFilename(cfg.Name)
	prefix := "recordings/" + safeName + "/"

	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		5*time.Minute,
		errors.New("s3 cleanup timeout"),
	)
	defer cancel()

	var deleted int
	var continuationToken *string

	for {
		input := &s3.ListObjectsV2Input{
			Bucket: aws.String(cfg.S3Bucket),
			Prefix: aws.String(prefix),
		}
		if continuationToken != nil {
			input.ContinuationToken = continuationToken
		}

		output, err := client.ListObjectsV2(ctx, input)
		if err != nil {
			slog.Warn("cleanup: failed to list S3 objects", "id", cfg.ID, "bucket", cfg.S3Bucket, "error", err)
			return
		}

		for _, obj := range output.Contents {
			key := aws.ToString(obj.Key)
			filename := filepath.Base(key)

			// Extract date from filename
			fileDate, ok := util.ExtractDateFromFilename(filename)
			if !ok {
				continue
			}

			// Delete if older than retention
			if fileDate.Before(cutoff) {
				_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(cfg.S3Bucket),
					Key:    obj.Key,
				})
				if err != nil {
					slog.Warn("cleanup: failed to delete S3 object", "id", cfg.ID, "key", key, "error", err)
				} else {
					deleted++
					slog.Debug("cleanup: deleted S3 object", "id", cfg.ID, "key", key)
				}
			}
		}

		if !aws.ToBool(output.IsTruncated) {
			break
		}
		continuationToken = output.NextContinuationToken
	}

	if deleted > 0 {
		slog.Info("cleanup: deleted S3 objects", "id", cfg.ID, "count", deleted)
		m.logCleanupEvent(cfg.Name, deleted, "s3")
	}
}

// logCleanupEvent logs a cleanup event to the event log.
func (m *Manager) logCleanupEvent(recorderName string, filesDeleted int, storageType string) {
	if m.eventLogger == nil {
		return
	}
	if err := m.eventLogger.LogRecorder(eventlog.CleanupCompleted, &eventlog.RecorderEventParams{
		RecorderName: recorderName,
		FilesDeleted: filesDeleted,
		StorageType:  storageType,
	}); err != nil {
		slog.Warn("failed to log cleanup event", "error", err)
	}
}
