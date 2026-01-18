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
	req, ok := r.prepareUploadRequest(filePath)
	if !ok {
		return
	}

	select {
	case r.uploadQueue <- req:
		slog.Info("queued file for upload", "id", r.id, "file", filepath.Base(filePath))
		r.logUploadEvent(eventlog.UploadQueued, filepath.Base(filePath), req.s3Key, "", 0)
	default:
		slog.Warn("upload queue full", "id", r.id)
	}
}

// uploadDirectly uploads a file synchronously, bypassing the queue.
// Used by cleanupAfterWriteError to avoid race conditions with Stop().
func (r *GenericRecorder) uploadDirectly(filePath string) {
	req, ok := r.prepareUploadRequest(filePath)
	if !ok {
		return
	}

	slog.Info("uploading file directly (error recovery)", "id", r.id, "file", filepath.Base(filePath))
	r.uploadFile(req)
}

// prepareUploadRequest validates and creates an upload request for the given file.
// Returns false if the file should not be uploaded.
func (r *GenericRecorder) prepareUploadRequest(filePath string) (uploadRequest, bool) {
	info, err := os.Stat(filePath)
	if err != nil {
		slog.Warn("failed to stat recording file", "id", r.id, "error", err)
		return uploadRequest{}, false
	}

	// Local-only mode: no upload needed
	if r.config.StorageMode == types.StorageLocal {
		slog.Info("local storage mode, file saved", "id", r.id, "path", filePath)
		return uploadRequest{}, false
	}

	// S3 or Both mode: need S3 config
	if !r.isS3Configured() {
		slog.Warn("S3 not configured but storage mode requires it", "id", r.id, "mode", r.config.StorageMode)
		return uploadRequest{}, false
	}

	return uploadRequest{
		localPath: filePath,
		s3Key:     r.generateS3Key(filepath.Base(filePath)),
		fileSize:  info.Size(),
	}, true
}

// logUploadEvent logs upload-related events with optional retry count.
func (r *GenericRecorder) logUploadEvent(eventType eventlog.EventType, filename, s3Key, errMsg string, retryCount int) {
	if r.eventLogger == nil {
		return
	}
	r.mu.RLock()
	p := r.captureLogParamsLocked()
	r.mu.RUnlock()

	p.Filename = filename
	p.S3Key = s3Key
	p.Error = errMsg
	p.RetryCount = retryCount
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
	err := r.doUpload(req)
	if err != nil {
		slog.Error("upload failed", "id", r.id, "s3_key", req.s3Key, "error", err)
		r.logUploadEvent(eventlog.UploadFailed, filepath.Base(req.localPath), req.s3Key, err.Error(), 0)
		r.addToRetryQueue(req, err.Error())
		return
	}

	slog.Info("upload completed", "id", r.id, "s3_key", req.s3Key)
	r.logUploadEvent(eventlog.UploadCompleted, filepath.Base(req.localPath), req.s3Key, "", 0)
	r.deleteIfS3Only(req.localPath)
}

// doUpload performs the actual S3 upload. Returns nil on success.
func (r *GenericRecorder) doUpload(req uploadRequest) error {
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		5*time.Minute,
		errors.New("s3 upload timeout"),
	)
	defer cancel()

	file, err := os.Open(req.localPath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn("failed to close file after upload", "id", r.id, "error", closeErr)
		}
	}()

	client, err := r.getOrCreateS3Client()
	if err != nil {
		return err
	}
	if client == nil {
		return errors.New("no S3 client available")
	}

	r.mu.RLock()
	bucket := r.config.S3Bucket
	r.mu.RUnlock()

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(req.s3Key),
		Body:          file,
		ContentLength: aws.Int64(req.fileSize),
		ContentType:   aws.String(r.getContentType()),
	})
	return err
}

// deleteIfS3Only removes the local file if storage mode is S3-only.
func (r *GenericRecorder) deleteIfS3Only(localPath string) {
	r.mu.RLock()
	storageMode := r.config.StorageMode
	r.mu.RUnlock()

	if storageMode != types.StorageS3 {
		return
	}

	if err := os.Remove(localPath); err != nil {
		slog.Warn("failed to delete temp file after upload", "id", r.id, "path", localPath, "error", err)
	} else {
		slog.Debug("deleted temp file after upload", "id", r.id, "path", localPath)
	}
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
				"attempts", p.retryCount+1,
				"last_error", p.lastError)
			r.logUploadEvent(eventlog.UploadAbandoned, filepath.Base(p.request.localPath), p.request.s3Key, p.lastError, p.retryCount)
			continue
		}

		// Attempt retry
		p.retryCount++
		slog.Info("retrying upload",
			"id", r.id,
			"file", filepath.Base(p.request.localPath),
			"attempt", p.retryCount)
		r.logUploadEvent(eventlog.UploadRetry, filepath.Base(p.request.localPath), p.request.s3Key, "", p.retryCount)

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
	// Check if file still exists before attempting upload
	if _, err := os.Stat(p.request.localPath); os.IsNotExist(err) {
		slog.Warn("retry file no longer exists", "id", r.id, "path", p.request.localPath)
		return true // Nothing to upload
	}

	err := r.doUpload(p.request)
	if err != nil {
		p.lastError = err.Error()
		slog.Error("retry upload failed", "id", r.id, "s3_key", p.request.s3Key, "error", err)
		r.logUploadEvent(eventlog.UploadFailed, filepath.Base(p.request.localPath), p.request.s3Key, err.Error(), p.retryCount)
		return false
	}

	slog.Info("retry upload completed", "id", r.id, "s3_key", p.request.s3Key)
	r.logUploadEvent(eventlog.UploadCompleted, filepath.Base(p.request.localPath), p.request.s3Key, "", p.retryCount)
	r.deleteIfS3Only(p.request.localPath)
	return true
}
