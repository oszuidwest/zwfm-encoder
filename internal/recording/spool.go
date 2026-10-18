package recording

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const pendingUploadMetadataSuffix = ".upload.json"

type pendingUploadMetadata struct {
	RecorderID        string    `json:"recorder_id"`
	LocalPath         string    `json:"local_path"`
	S3Key             string    `json:"s3_key"`
	BaseKey           string    `json:"base_s3_key,omitempty"`
	UploadID          string    `json:"upload_id,omitempty"`
	FileSize          int64     `json:"file_size"`
	DeleteAfterUpload bool      `json:"delete_after_upload"`
	FirstAttempt      time.Time `json:"first_attempt"`
	RetryCount        int       `json:"retry_count"`
	LastError         string    `json:"last_error,omitempty"`
	Abandoned         bool      `json:"abandoned,omitempty"`
}

func (r *GenericRecorder) reconcilePendingUploads() error {
	pending, err := r.loadPendingUploads()
	if err != nil {
		return err
	}

	recovered, err := r.recoverOrphanSpoolFiles(pending)
	if err != nil {
		return err
	}
	pending = append(pending, recovered...)

	if len(pending) == 0 {
		return nil
	}

	r.mu.Lock()
	r.retryQueue = append(r.retryQueue, pending...)
	r.mu.Unlock()

	slog.Info("restored pending recording uploads", "id", r.id, "count", len(pending))
	return nil
}

func (r *GenericRecorder) loadPendingUploads() ([]pendingUpload, error) {
	entries, err := os.ReadDir(r.pendingUploadDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []pendingUpload{}, nil
		}
		return nil, fmt.Errorf("read pending upload directory: %w", err)
	}

	pending := make([]pendingUpload, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), pendingUploadMetadataSuffix) {
			continue
		}

		metadataPath := filepath.Join(r.pendingUploadDir(), entry.Name())
		item, ok, err := r.loadPendingUpload(metadataPath)
		if err != nil {
			return nil, err
		}
		if ok {
			pending = append(pending, item)
		}
	}
	return pending, nil
}

func (r *GenericRecorder) loadPendingUpload(metadataPath string) (pendingUpload, bool, error) {
	data, err := os.ReadFile(metadataPath) //nolint:gosec // Path comes from the recorder-owned spool directory.
	if err != nil {
		return pendingUpload{}, false, fmt.Errorf("read pending upload metadata: %w", err)
	}

	var meta pendingUploadMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		slog.Warn("removing invalid pending upload metadata", "id", r.id, "path", metadataPath, "error", err)
		return pendingUpload{}, false, os.Remove(metadataPath)
	}
	if meta.RecorderID != "" && meta.RecorderID != r.id {
		return pendingUpload{}, false, nil
	}
	if meta.LocalPath == "" || meta.S3Key == "" {
		slog.Warn("removing incomplete pending upload metadata", "id", r.id, "path", metadataPath)
		return pendingUpload{}, false, os.Remove(metadataPath)
	}
	info, ok, err := finalizedRecording(meta.LocalPath)
	if errors.Is(err, os.ErrNotExist) {
		slog.Warn("removing pending upload metadata for missing file", "id", r.id, "path", meta.LocalPath)
		return pendingUpload{}, false, os.Remove(metadataPath)
	}
	if err != nil {
		return pendingUpload{}, false, fmt.Errorf("stat pending upload file: %w", err)
	}
	if !ok {
		return pendingUpload{}, false, nil
	}
	if meta.FileSize == 0 {
		meta.FileSize = info.Size()
	}
	// Restart also re-queues uploads whose retry window expired while offline.
	if meta.FirstAttempt.IsZero() || meta.Abandoned || time.Since(meta.FirstAttempt) > MaxUploadRetryAge {
		meta.FirstAttempt = time.Now()
		meta.RetryCount = 0
	}
	if meta.BaseKey == "" {
		// Older metadata only kept the last key tried; the local filename is the base.
		meta.BaseKey = path.Join(path.Dir(meta.S3Key), filepath.Base(meta.LocalPath))
	}

	return pendingUpload{
		request: uploadRequest{
			localPath:         meta.LocalPath,
			s3Key:             meta.S3Key,
			fileSize:          meta.FileSize,
			deleteAfterUpload: meta.DeleteAfterUpload,
		},
		baseKey:      meta.BaseKey,
		uploadID:     meta.UploadID,
		firstAttempt: meta.FirstAttempt,
		retryCount:   meta.RetryCount,
		lastError:    meta.LastError,
		metadataPath: metadataPath,
	}, true, nil
}

func (r *GenericRecorder) recoverOrphanSpoolFiles(existing []pendingUpload) ([]pendingUpload, error) {
	cfg := r.Config()
	if cfg.StorageMode != types.StorageS3 {
		return []pendingUpload{}, nil
	}

	dir := filepath.Join(r.spoolDir, "recorders", r.id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []pendingUpload{}, nil
		}
		return nil, fmt.Errorf("read recorder spool directory: %w", err)
	}

	known := make(map[string]struct{}, len(existing))
	for i := range existing {
		known[existing[i].request.localPath] = struct{}{}
	}

	recovered := []pendingUpload{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if recording, ok := strings.CutSuffix(entry.Name(), unfinalizedRecordingSuffix); ok {
			slog.Warn("recording kept in quarantine for inspection; it is not uploaded automatically",
				"id", r.id, "file", filepath.Join(dir, recording))
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		if _, ok := known[filePath]; ok {
			continue
		}
		if _, owned := recorderFileTime(&cfg, entry.Name()); !owned {
			continue // A shared/case-aliased spool directory does not prove ownership.
		}

		info, ok, err := finalizedRecording(filePath)
		if err != nil {
			return nil, fmt.Errorf("stat recorder spool file: %w", err)
		}
		if !ok {
			continue
		}
		pending := pendingUpload{
			request: uploadRequest{
				localPath:         filePath,
				s3Key:             s3ObjectKey(&cfg, entry.Name()),
				fileSize:          info.Size(),
				deleteAfterUpload: true,
			},
			firstAttempt: time.Now(),
			lastError:    "recovered from spool without upload metadata",
		}
		if err := r.savePendingUpload(&pending); err != nil {
			return nil, err
		}
		recovered = append(recovered, pending)
	}
	return recovered, nil
}

func (r *GenericRecorder) savePendingUpload(p *pendingUpload) error {
	if p.baseKey == "" {
		p.baseKey = p.request.s3Key
	}
	if p.uploadID == "" {
		p.uploadID = rand.Text()
	}
	if p.firstAttempt.IsZero() {
		p.firstAttempt = time.Now()
	}
	if p.metadataPath == "" {
		p.metadataPath = r.pendingUploadMetadataPath(p)
	}
	if err := os.MkdirAll(filepath.Dir(p.metadataPath), 0o755); err != nil { //nolint:gosec // Spool metadata directory needs to be readable.
		return fmt.Errorf("create pending upload directory: %w", err)
	}

	meta := pendingUploadMetadata{
		RecorderID:        r.id,
		LocalPath:         p.request.localPath,
		S3Key:             p.request.s3Key,
		BaseKey:           p.baseKey,
		UploadID:          p.uploadID,
		FileSize:          p.request.fileSize,
		DeleteAfterUpload: p.request.deleteAfterUpload,
		FirstAttempt:      p.firstAttempt,
		RetryCount:        p.retryCount,
		LastError:         p.lastError,
		Abandoned:         p.abandoned,
	}

	data, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pending upload metadata: %w", err)
	}

	tmp := p.metadataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write pending upload metadata: %w", err)
	}
	if err := os.Rename(tmp, p.metadataPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace pending upload metadata: %w", err)
	}
	return nil
}

func (r *GenericRecorder) removePendingMetadata(p *pendingUpload) {
	if p.metadataPath == "" {
		return
	}
	if err := os.Remove(p.metadataPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove pending upload metadata", "id", r.id, "path", p.metadataPath, "error", err)
	}
}

func (r *GenericRecorder) pendingUploadDir() string {
	return filepath.Join(r.spoolDir, "uploads", r.id)
}

func (r *GenericRecorder) pendingUploadMetadataPath(p *pendingUpload) string {
	sum := sha256.Sum256([]byte(p.request.localPath + "\x00" + p.baseKey))
	return filepath.Join(r.pendingUploadDir(), hex.EncodeToString(sum[:8])+pendingUploadMetadataSuffix)
}
