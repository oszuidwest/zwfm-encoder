package recording

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// uploadRequest is a request to upload a file to S3.
type uploadRequest struct {
	localPath string
	s3Key     string
	fileSize  int64
}

// queueForUpload adds a completed file to the upload queue.
func (r *GenericRecorder) queueForUpload(filePath string) {
	info, err := os.Stat(filePath)
	if err != nil {
		slog.Warn("failed to stat recording file", "id", r.id, "error", err)
		return
	}

	// Local-only mode: no upload needed
	if r.config.StorageMode == types.StorageLocal {
		slog.Info("local storage mode, file saved", "id", r.id, "path", filePath)
		return
	}

	// S3 or Both mode: queue for upload
	if !r.isS3Configured() {
		slog.Warn("S3 not configured but storage mode requires it", "id", r.id, "mode", r.config.StorageMode)
		return
	}

	s3Key := r.generateS3Key(filepath.Base(filePath))

	select {
	case r.uploadQueue <- uploadRequest{
		localPath: filePath,
		s3Key:     s3Key,
		fileSize:  info.Size(),
	}:
		slog.Info("queued file for upload", "id", r.id, "file", filepath.Base(filePath))
		r.logUploadEventLocked(eventlog.UploadQueued, filepath.Base(filePath), s3Key, "")
	default:
		slog.Warn("upload queue full", "id", r.id)
	}
}

// logUploadEventLocked logs an upload-related event, acquiring the lock to capture config.
func (r *GenericRecorder) logUploadEventLocked(eventType eventlog.EventType, filename, s3Key, errMsg string) {
	if r.eventLogger == nil {
		return
	}
	r.mu.RLock()
	ctx := r.captureLogContextLocked()
	r.mu.RUnlock()

	r.logEvent(ctx, eventType, &logParams{
		filename: filename,
		s3Key:    s3Key,
		errMsg:   errMsg,
	})
}

// uploadWorker processes the upload queue.
func (r *GenericRecorder) uploadWorker() {
	defer r.uploadWg.Done()

	for {
		select {
		case <-r.uploadStopCh:
			// Drain remaining items before exiting
			for {
				select {
				case req := <-r.uploadQueue:
					r.uploadFile(req)
				default:
					return
				}
			}
		case req := <-r.uploadQueue:
			r.uploadFile(req)
		}
	}
}

// uploadFile uploads a single file to S3.
func (r *GenericRecorder) uploadFile(req uploadRequest) {
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		5*time.Minute,
		errors.New("s3 upload timeout"),
	)
	defer cancel()

	file, err := os.Open(req.localPath)
	if err != nil {
		slog.Error("failed to open file for upload", "id", r.id, "error", err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Warn("failed to close file after upload", "id", r.id, "error", err)
		}
	}()

	// Get S3 client (recreates if config changed - same pattern as Graph client)
	client, err := r.getOrCreateS3Client()
	if err != nil {
		slog.Error("failed to create S3 client", "id", r.id, "error", err)
		return
	}
	if client == nil {
		slog.Warn("no S3 client available", "id", r.id)
		return
	}

	r.mu.RLock()
	bucket := r.config.S3Bucket
	r.mu.RUnlock()

	contentType := r.getContentType()

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(req.s3Key),
		Body:          file,
		ContentLength: aws.Int64(req.fileSize),
		ContentType:   aws.String(contentType),
	})

	if err != nil {
		slog.Error("upload failed", "id", r.id, "s3_key", req.s3Key, "error", err)
		r.logUploadEventLocked(eventlog.UploadFailed, filepath.Base(req.localPath), req.s3Key, err.Error())
		return
	}

	r.mu.RLock()
	storageMode := r.config.StorageMode
	r.mu.RUnlock()

	slog.Info("upload completed", "id", r.id, "s3_key", req.s3Key)
	r.logUploadEventLocked(eventlog.UploadCompleted, filepath.Base(req.localPath), req.s3Key, "")

	// Handle local file based on storage mode
	if storageMode == types.StorageS3 {
		// S3-only: delete temp file immediately after successful upload
		if err := os.Remove(req.localPath); err != nil {
			slog.Warn("failed to delete temp file after upload", "id", r.id, "path", req.localPath, "error", err)
		} else {
			slog.Debug("deleted temp file after upload", "id", r.id, "path", req.localPath)
		}
	}
	// For "both" mode: file stays in LocalPath until retention cleanup
}
