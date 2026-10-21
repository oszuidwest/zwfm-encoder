package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// uploadRequest represents a file to be uploaded to S3.
type uploadRequest struct {
	localPath         string
	s3Key             string
	fileSize          int64
	deleteAfterUpload bool
}

// pendingUpload carries one upload's identity and retry state.
type pendingUpload struct {
	request      uploadRequest
	baseKey      string
	uploadID     string
	firstAttempt time.Time
	retryCount   int
	lastError    string
	metadataPath string
	abandoned    bool
}

// MaxUploadRetryAge is the maximum age for retrying uploads.
const MaxUploadRetryAge = 24 * time.Hour

// uploadStoppedReason records an upload that Stop left to the retry queue;
// processRetryQueue reports these as upload_queued. It is persisted in upload
// metadata, so keep the value stable.
const uploadStoppedReason = "upload worker stopped"

func (r *GenericRecorder) queueForUpload(filePath string) {
	req, ok := r.prepareUploadRequest(filePath)
	if !ok {
		return
	}

	// Serialize with Stop so every file reaches worker or retry storage.
	r.mu.Lock()
	closed := r.uploadClosed
	queued := false
	if !closed {
		select {
		case r.uploadQueue <- req:
			queued = true
		default:
		}
	}
	r.mu.Unlock()

	if queued {
		slog.Info("queued file for upload", "id", r.id, "file", filepath.Base(filePath))
		r.logUploadEvent(eventlog.UploadQueued, filepath.Base(filePath), req.s3Key, "", 0)
		return
	}

	// In S3 modes every stop sends its final file this way; only a full queue
	// is a problem.
	reason, log := "upload queue full", slog.Warn
	if closed {
		reason, log = uploadStoppedReason, slog.Info
	}
	log("queuing upload via retry queue", "id", r.id, "reason", reason)
	r.addToRetryQueue(req, reason)
}

// prepareUploadRequest validates and creates an upload request for the given file.
// Returns false if the file should not be uploaded.
func (r *GenericRecorder) prepareUploadRequest(filePath string) (uploadRequest, bool) {
	if err := util.ValidatePath("recording upload path", filePath); err != nil {
		slog.Warn("invalid recording upload path", "id", r.id, "path", filePath, "error", err)
		return uploadRequest{}, false
	}
	info, ok, err := finalizedRecording(filePath)
	if !ok {
		slog.Warn("recording is not uploadable; upload skipped", "id", r.id, "path", filePath, "error", err)
		return uploadRequest{}, false
	}

	r.mu.RLock()
	storageMode := r.config.StorageMode
	objectKey := s3ObjectKey(&r.config, filepath.Base(filePath))
	s3Configured := r.config.HasS3Credentials()
	r.mu.RUnlock()

	if storageMode == types.StorageLocal {
		slog.Info("local storage mode, file saved", "id", r.id, "path", filePath)
		return uploadRequest{}, false
	}

	if !s3Configured {
		slog.Warn("S3 not configured but storage mode requires it", "id", r.id, "mode", storageMode)
		return uploadRequest{}, false
	}

	return uploadRequest{
		localPath:         filePath,
		s3Key:             objectKey,
		fileSize:          info.Size(),
		deleteAfterUpload: storageMode == types.StorageS3,
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

// startUploadWorkerLocked starts the upload worker of a run once. Must be
// called with r.mu held.
func (r *GenericRecorder) startUploadWorkerLocked() {
	if r.cancelUploads != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancelUploads = cancel
	r.uploadClosed = false // Reopen upload intake.
	r.uploadWg.Go(func() { r.uploadWorker(ctx) })
}

// uploadWorker processes the upload queue until Stop cancels ctx. Stop must
// not wait for S3, so the cancelled upload and the files still queued move to
// the persisted retry queue, and the retry pass Stop starts uploads them.
func (r *GenericRecorder) uploadWorker(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case req := <-r.uploadQueue:
			// Stop cancels only after finalizing: do not start an upload it
			// would interrupt.
			r.mu.RLock()
			closed := r.uploadClosed
			r.mu.RUnlock()
			if closed {
				r.addToRetryQueue(req, uploadStoppedReason)
			} else {
				r.uploadFile(ctx, req)
			}
		}
	}
	// Stop closes intake before it cancels, so the queue can only shrink.
	for len(r.uploadQueue) > 0 {
		r.addToRetryQueue(<-r.uploadQueue, uploadStoppedReason)
	}
}

// uploadFile uploads to S3 and deletes temp files in S3-only mode.
func (r *GenericRecorder) uploadFile(ctx context.Context, req uploadRequest) {
	pending := pendingUpload{request: req}
	if !r.uploadPending(ctx, &pending) {
		r.enqueuePendingUpload(&pending)
	}
}

// maxS3KeySuffix bounds the suffixed keys doUpload tries; each attempt sends
// the whole file.
const maxS3KeySuffix = 10

// doUpload uploads with If-None-Match: * so an existing object is never
// replaced. When the key is taken it retries under -2, -3, ... within the same
// timeout; p.request.s3Key is left at the last key tried. Identity is persisted
// before writes so a committed upload can be recognized after a lost response.
func (r *GenericRecorder) doUpload(ctx context.Context, p *pendingUpload) error {
	req := &p.request
	// Also for an upload queued before its file was marked.
	if _, ok, err := finalizedRecording(req.localPath); err != nil {
		return fmt.Errorf("open recording for upload: %w", err)
	} else if !ok {
		return errRecordingNotUploadable
	}
	file, err := os.Open(req.localPath)
	if err != nil {
		return fmt.Errorf("open recording for upload: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn("failed to close file after upload", "id", r.id, "error", closeErr)
		}
	}()

	client := r.getOrCreateS3Client()
	if client == nil {
		return errNoS3Client
	}

	r.mu.RLock()
	bucket := r.config.S3Bucket
	contentType := r.config.Codec.ContentType()
	r.mu.RUnlock()

	// Create transfer manager with progress logging
	tm := transfermanager.New(client, func(o *transfermanager.Options) {
		o.Concurrency = 5
		o.PartSizeBytes = 8 * 1024 * 1024 // 8 MiB parts
		// Abort a multipart upload on a fresh context: a stop cancels ctx,
		// and an unaborted upload keeps its parts stored on S3.
		o.FailTimeout = 5 * time.Second
		o.ObjectProgressListeners.Register(&uploadProgressListener{
			id:       r.id,
			filename: filepath.Base(req.localPath),
			fileSize: req.fileSize,
		})
	})

	// Use a generous timeout: 2 minutes per 50MB, minimum 10 minutes
	timeoutMinutes := max(10, int(req.fileSize/(50*1024*1024))*2+2)
	ctx, cancel := context.WithTimeoutCause(
		ctx,
		time.Duration(timeoutMinutes)*time.Minute,
		errors.New("s3 upload timeout"),
	)
	defer cancel()

	slog.Info("starting upload",
		"id", r.id,
		"file", filepath.Base(req.localPath),
		"size_mb", req.fileSize/(1024*1024),
		"timeout_min", timeoutMinutes)

	if p.baseKey == "" {
		p.baseKey = req.s3Key
	}
	suffix := 2
	for candidate := 2; candidate <= maxS3KeySuffix; candidate++ {
		if req.s3Key == withNumericSuffix(p.baseKey, candidate) {
			suffix = candidate + 1
			break
		}
	}
	for ; ; suffix++ {
		if err := r.savePendingUpload(p); err != nil {
			return fmt.Errorf("persist upload identity: %w", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind recording for upload: %w", err)
		}

		_, err = tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(req.s3Key),
			Body:          file,
			ContentLength: aws.Int64(req.fileSize),
			ContentType:   aws.String(contentType),
			IfNoneMatch:   aws.String("*"),
			Metadata:      map[string]string{"zwfm-upload-id": p.uploadID},
		})
		if err == nil {
			return nil
		}
		// CompleteMultipartUpload can be committed even if an SDK retry returns NoSuchUpload.
		head, headErr := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(req.s3Key)})
		if headErr == nil && head.Metadata["zwfm-upload-id"] == p.uploadID {
			return nil
		}
		if !isS3KeyConflict(err) {
			return err
		}
		if headErr != nil {
			return fmt.Errorf("verify conflicting S3 object: %w", headErr)
		}
		if suffix > maxS3KeySuffix {
			return fmt.Errorf("no free S3 key for %s after %d attempts: %w", p.baseKey, maxS3KeySuffix, err)
		}

		previousKey := req.s3Key
		req.s3Key = withNumericSuffix(p.baseKey, suffix)
		slog.Warn("S3 key taken, using suffixed key",
			"id", r.id,
			"taken_key", previousKey,
			"new_key", req.s3Key)
	}
}

// isS3KeyConflict reports whether a conditional upload failed because the key
// is taken or another conditional write to it is in progress. The code also
// covers CompleteMultipartUpload, which reports errors in an HTTP 200 body;
// the status covers a 412 whose body has no parsable code.
func isS3KeyConflict(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		if code := apiErr.ErrorCode(); code == "PreconditionFailed" || code == "ConditionalRequestConflict" {
			return true
		}
	}
	var responseErr interface{ HTTPStatusCode() int }
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusPreconditionFailed
}

var errNoS3Client = &noS3ClientError{}

var errRecordingNotUploadable = errors.New("recording is marked unfinalized, empty or not a regular file; upload skipped")

// Compile-time interface check.
var _ transfermanager.ObjectBytesTransferredListener = (*uploadProgressListener)(nil)

type noS3ClientError struct{}

func (e *noS3ClientError) Error() string { return "no S3 client available" }

// uploadProgressListener logs upload progress. Multipart parts report
// concurrently, so mu guards lastLoggedPct.
type uploadProgressListener struct {
	id            string
	filename      string
	fileSize      int64
	mu            sync.Mutex
	lastLoggedPct int
}

func (l *uploadProgressListener) OnObjectBytesTransferred(
	_ context.Context, e *transfermanager.ObjectBytesTransferredEvent,
) {
	if l.fileSize == 0 {
		return
	}
	pct := int(e.BytesTransferred * 100 / l.fileSize)
	l.mu.Lock()
	defer l.mu.Unlock()
	// Log every 25%
	if pct >= l.lastLoggedPct+25 {
		l.lastLoggedPct = pct
		slog.Debug("upload progress", "id", l.id, "file", l.filename, "progress", pct)
	}
}

// deleteAfterUpload removes spool-only files after successful upload.
func (r *GenericRecorder) deleteAfterUpload(req uploadRequest) bool {
	if !req.deleteAfterUpload {
		return true
	}

	if err := os.Remove(req.localPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to delete spool file after upload", "id", r.id, "path", req.localPath, "error", err)
		return false
	}
	slog.Debug("deleted spool file after upload", "id", r.id, "path", req.localPath)
	return true
}

// addToRetryQueue adds a failed or deferred upload to the retry queue.
func (r *GenericRecorder) addToRetryQueue(req uploadRequest, errMsg string) {
	r.enqueuePendingUpload(&pendingUpload{request: req, lastError: errMsg})
}

// enqueuePendingUpload preserves the identity/window of an already attempted upload.
func (r *GenericRecorder) enqueuePendingUpload(pending *pendingUpload) {
	r.retryEnqueueMu.Lock()
	defer r.retryEnqueueMu.Unlock()
	r.mu.RLock()

	if _, exists := r.retryInFlight[pending.request.localPath]; exists {
		r.mu.RUnlock()
		return
	}
	for i := range r.retryQueue {
		if r.retryQueue[i].request.localPath == pending.request.localPath {
			r.mu.RUnlock()
			return
		}
	}
	r.mu.RUnlock()

	// Publish the persisted identity, never a copy with an uninitialized token.
	if err := r.savePendingUpload(pending); err != nil {
		slog.Error("failed to persist pending upload", "id", r.id, "file", filepath.Base(pending.request.localPath), "error", err)
	}
	r.mu.Lock()
	r.retryQueue = append(r.retryQueue, *pending)
	r.mu.Unlock()
	slog.Info("upload queued for retry", "id", r.id, "file", filepath.Base(pending.request.localPath))
}

// processRetryQueue gives one batch exclusive ownership of pending paths.
// An overlapping call requests a follow-up pass and returns immediately.
func (r *GenericRecorder) processRetryQueue() {
	r.mu.Lock()
	if !r.retryProcessMu.TryLock() {
		// Another batch owns this recorder's uploads; it runs once more afterwards.
		r.retryRequested = true
		r.mu.Unlock()
		return
	}
	defer func() {
		r.mu.Lock()
		again := r.retryRequested
		r.retryRequested = false
		r.retryProcessMu.Unlock()
		r.mu.Unlock()
		if again {
			go r.processRetryQueue()
		}
	}()
	if len(r.retryQueue) == 0 {
		r.mu.Unlock()
		return
	}

	pending := slices.Clone(r.retryQueue)
	r.retryQueue = nil
	if r.retryInFlight == nil {
		r.retryInFlight = make(map[string]struct{})
	}
	for i := range pending {
		r.retryInFlight[pending[i].request.localPath] = struct{}{}
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		for i := range pending {
			delete(r.retryInFlight, pending[i].request.localPath)
		}
		r.mu.Unlock()
	}()

	now := time.Now()

	for i := range pending {
		p := &pending[i]

		if now.Sub(p.firstAttempt) > MaxUploadRetryAge {
			slog.Warn("upload abandoned after 24h",
				"id", r.id,
				"file", filepath.Base(p.request.localPath),
				"attempts", p.retryCount+1,
				"last_error", p.lastError)
			r.logUploadEvent(eventlog.UploadAbandoned,
				filepath.Base(p.request.localPath), p.request.s3Key,
				p.lastError, p.retryCount)
			p.abandoned = true
			if err := r.savePendingUpload(p); err != nil {
				slog.Error("failed to persist abandoned upload", "id", r.id, "file", filepath.Base(p.request.localPath), "error", err)
			}
			if r.onUploadAbandoned != nil {
				r.mu.RLock()
				recorderName := r.config.Name
				r.mu.RUnlock()
				r.onUploadAbandoned(UploadAbandonedEvent{
					RecorderName: recorderName,
					LocalPath:    p.request.localPath,
					Filename:     filepath.Base(p.request.localPath),
					S3Key:        p.request.s3Key,
					LastError:    p.lastError,
					RetryCount:   p.retryCount,
				})
			}
			continue
		}
		if _, err := os.Stat(p.request.localPath); os.IsNotExist(err) {
			slog.Warn("retry file no longer exists", "id", r.id, "path", p.request.localPath)
			r.removePendingMetadata(p)
			continue
		}

		// A stop deferred this upload: its first attempt is neither a retry
		// nor a failure to warn about.
		event := eventlog.UploadQueued
		if p.lastError != uploadStoppedReason {
			p.retryCount++
			event = eventlog.UploadRetry
		}
		slog.Info("retrying upload",
			"id", r.id,
			"file", filepath.Base(p.request.localPath),
			"attempt", p.retryCount)
		r.logUploadEvent(event, filepath.Base(p.request.localPath), p.request.s3Key, "", p.retryCount)

		if !r.uploadPending(context.Background(), p) {
			if err := r.savePendingUpload(p); err != nil {
				slog.Error("failed to persist pending upload", "id", r.id, "file", filepath.Base(p.request.localPath), "error", err)
			}
			r.mu.Lock()
			r.retryQueue = append(r.retryQueue, *p)
			r.mu.Unlock()
		}
	}
}

// uploadPending performs the upload and returns true when no retry is needed.
func (r *GenericRecorder) uploadPending(ctx context.Context, p *pendingUpload) bool {
	err := r.doUpload(ctx, p)
	if err != nil && ctx.Err() != nil {
		// Stopped, not failed. The identity saved before the write lets the
		// retry recognize an upload that was committed anyway.
		p.lastError = uploadStoppedReason
		slog.Info("upload interrupted by stop, left for retry", "id", r.id, "file", filepath.Base(p.request.localPath), "error", err)
		return false
	}
	if err != nil {
		p.lastError = err.Error()
		slog.Error("upload failed", "id", r.id, "s3_key", p.request.s3Key, "error", err)
		r.logUploadEvent(eventlog.UploadFailed,
			filepath.Base(p.request.localPath), p.request.s3Key,
			err.Error(), p.retryCount)
		return false
	}

	slog.Info("upload completed", "id", r.id, "s3_key", p.request.s3Key)
	r.logUploadEvent(eventlog.UploadCompleted, filepath.Base(p.request.localPath), p.request.s3Key, "", p.retryCount)
	// Keep the identity while a spool copy remains, so a restart recognizes the upload.
	if r.deleteAfterUpload(p.request) {
		r.removePendingMetadata(p)
	}
	return true
}
