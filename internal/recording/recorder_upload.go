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

// uploadRequest represents a file to be uploaded to S3.
type uploadRequest struct {
	localPath string
	s3Key     string
	fileSize  int64
}

// pendingUpload tracks a failed upload for retry.
type pendingUpload struct {
	request      uploadRequest
	firstAttempt time.Time
	retryCount   int
	lastError    string
}

// MaxUploadRetryAge is the maximum age for retrying uploads.
const MaxUploadRetryAge = 24 * time.Hour

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

func (r *GenericRecorder) logUploadEventLocked(eventType eventlog.EventType, filename, s3Key, errMsg string) {
	if r.eventLogger == nil {
		return
	}
	r.mu.RLock()
	p := r.captureLogParamsLocked()
	r.mu.RUnlock()

	p.Filename = filename
	p.S3Key = s3Key
	p.Error = errMsg
	r.logEvent(eventType, p)
}

// uploadWorker processes the upload queue, draining remaining items on shutdown.
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

// uploadFile uploads to S3 and deletes temp files in S3-only mode.
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
		r.addToRetryQueue(req, err.Error())
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

// addToRetryQueue adds a failed upload to the retry queue.
func (r *GenericRecorder) addToRetryQueue(req uploadRequest, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Prevent duplicates
	for _, p := range r.retryQueue {
		if p.request.localPath == req.localPath {
			return
		}
	}

	r.retryQueue = append(r.retryQueue, pendingUpload{
		request:      req,
		firstAttempt: time.Now(),
		retryCount:   0,
		lastError:    errMsg,
	})

	slog.Info("upload queued for retry", "id", r.id, "file", filepath.Base(req.localPath))
}

// processRetryQueue attempts to upload all pending files.
// Called from rotateFile() at hour boundaries.
func (r *GenericRecorder) processRetryQueue() {
	r.mu.Lock()
	if len(r.retryQueue) == 0 {
		r.mu.Unlock()
		return
	}

	// Copy and clear queue
	pending := make([]pendingUpload, len(r.retryQueue))
	copy(pending, r.retryQueue)
	r.retryQueue = nil
	r.mu.Unlock()

	now := time.Now()

	for i := range pending {
		p := &pending[i]

		// Check 24-hour limit
		if now.Sub(p.firstAttempt) > MaxUploadRetryAge {
			slog.Warn("upload abandoned after 24h",
				"id", r.id,
				"file", filepath.Base(p.request.localPath),
				"attempts", p.retryCount+1)
			r.logRetryEvent(eventlog.UploadAbandoned, p, "exceeded 24h retry limit")
			continue
		}

		// Attempt retry
		p.retryCount++
		slog.Info("retrying upload",
			"id", r.id,
			"file", filepath.Base(p.request.localPath),
			"attempt", p.retryCount)
		r.logRetryEvent(eventlog.UploadRetry, p, "")

		if !r.retryUpload(p) {
			// Failed - re-add to queue
			r.mu.Lock()
			r.retryQueue = append(r.retryQueue, *p)
			r.mu.Unlock()
		}
	}
}

// retryUpload performs the upload and returns true on success.
func (r *GenericRecorder) retryUpload(p *pendingUpload) bool {
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		5*time.Minute,
		errors.New("s3 upload timeout"),
	)
	defer cancel()

	file, err := os.Open(p.request.localPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("retry file no longer exists", "id", r.id, "path", p.request.localPath)
			return true // Nothing to upload
		}
		p.lastError = err.Error()
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Warn("failed to close file after retry", "id", r.id, "error", err)
		}
	}()

	client, err := r.getOrCreateS3Client()
	if err != nil || client == nil {
		if err != nil {
			p.lastError = err.Error()
		}
		return false
	}

	r.mu.RLock()
	bucket := r.config.S3Bucket
	storageMode := r.config.StorageMode
	r.mu.RUnlock()

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(p.request.s3Key),
		Body:          file,
		ContentLength: aws.Int64(p.request.fileSize),
		ContentType:   aws.String(r.getContentType()),
	})

	if err != nil {
		p.lastError = err.Error()
		slog.Error("retry upload failed", "id", r.id, "s3_key", p.request.s3Key, "error", err)
		r.logUploadEventLocked(eventlog.UploadFailed, filepath.Base(p.request.localPath), p.request.s3Key, err.Error())
		return false
	}

	slog.Info("retry upload completed", "id", r.id, "s3_key", p.request.s3Key)
	r.logUploadEventLocked(eventlog.UploadCompleted, filepath.Base(p.request.localPath), p.request.s3Key, "")

	// Delete temp file if S3-only mode
	if storageMode == types.StorageS3 {
		if err := os.Remove(p.request.localPath); err != nil {
			slog.Warn("failed to delete temp file after retry upload", "id", r.id, "path", p.request.localPath, "error", err)
		}
	}

	return true
}

// logRetryEvent logs retry-related events with attempt count.
func (r *GenericRecorder) logRetryEvent(eventType eventlog.EventType, p *pendingUpload, errMsg string) {
	if r.eventLogger == nil {
		return
	}
	r.mu.RLock()
	params := r.captureLogParamsLocked()
	r.mu.RUnlock()

	params.Filename = filepath.Base(p.request.localPath)
	params.S3Key = p.request.s3Key
	params.RetryCount = p.retryCount
	params.Error = errMsg
	r.logEvent(eventType, params)
}
