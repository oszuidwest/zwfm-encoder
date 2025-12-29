package recording

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GenericRecorder handles recording to a file with S3 upload.
// It supports both hourly rotation and on-demand modes, and any codec.
type GenericRecorder struct {
	mu sync.RWMutex

	id                 string
	config             types.Recorder
	maxDurationMinutes int           // For on-demand mode (from global config)
	rotationOffset     time.Duration // Staggered offset for hourly rotation

	tempDir string
	state   RecordingState
	error   string

	// FFmpeg process
	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
	stdin  io.WriteCloser
	stderr *bytes.Buffer

	// Current recording
	currentFile  string
	startTime    time.Time
	bytesWritten int64

	// S3 client
	s3Client *s3.Client

	// Upload queue
	uploadQueue    chan uploadRequest
	uploadWg       sync.WaitGroup
	uploadStopCh   chan struct{}
	lastUploadTime *time.Time
	lastUploadErr  string

	// Rotation timer (hourly mode)
	rotationTimer *time.Timer

	// Max duration timer (on-demand mode)
	durationTimer *time.Timer

	// Callbacks
	statusCallback func()

	// File cleanup tracking
	cleanupMu     sync.Mutex
	uploadedFiles map[string]time.Time // path -> upload time
}

// uploadRequest represents a file ready for S3 upload.
type uploadRequest struct {
	localPath string
	s3Key     string
	fileSize  int64
}

// NewGenericRecorder creates a new recorder with the given configuration.
// maxDurationMinutes is the global max duration for on-demand recorders.
// rotationOffset is the staggered delay for hourly rotation (spread across 30s window).
func NewGenericRecorder(cfg *types.Recorder, tempDir string, maxDurationMinutes int, rotationOffset time.Duration, statusCallback func()) (*GenericRecorder, error) {
	r := &GenericRecorder{
		id:                 cfg.ID,
		config:             *cfg,
		maxDurationMinutes: maxDurationMinutes,
		rotationOffset:     rotationOffset,
		tempDir:            tempDir,
		state:              StateIdle,
		statusCallback:     statusCallback,
		uploadQueue:        make(chan uploadRequest, 100),
		uploadStopCh:       make(chan struct{}),
		uploadedFiles:      make(map[string]time.Time),
	}

	// Create S3 client if configured
	if r.isS3Configured() {
		client, err := r.createS3Client()
		if err != nil {
			return nil, fmt.Errorf("create S3 client: %w", err)
		}
		r.s3Client = client
	}

	return r, nil
}

// ID returns the recorder's unique identifier.
func (r *GenericRecorder) ID() string {
	return r.id
}

// Config returns a copy of the recorder's configuration.
func (r *GenericRecorder) Config() types.Recorder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config
}

// Start begins recording.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == StateRecording {
		return nil // Already recording
	}

	// Create temp directory for this recorder
	recorderDir := filepath.Join(r.tempDir, "recorders", r.id)
	if err := os.MkdirAll(recorderDir, 0o755); err != nil {
		return fmt.Errorf("create recorder directory: %w", err)
	}

	// Start FFmpeg encoder
	if err := r.startEncoderLocked(); err != nil {
		return err
	}

	// Start upload worker
	r.uploadWg.Add(1)
	go r.uploadWorker()

	// Start file cleanup worker
	r.uploadWg.Add(1)
	go r.cleanupWorker()

	// Schedule based on rotation mode
	if r.config.RotationMode == types.RotationHourly {
		r.scheduleRotationLocked()
	} else if r.maxDurationMinutes > 0 {
		r.scheduleDurationLimitLocked()
	}

	r.state = StateRecording
	r.error = ""

	slog.Info("recorder started", "id", r.id, "name", r.config.Name, "mode", r.config.RotationMode)
	return nil
}

// Stop gracefully stops recording.
func (r *GenericRecorder) Stop() error {
	r.mu.Lock()

	if r.state == StateIdle {
		r.mu.Unlock()
		return nil
	}

	r.state = StateFinalizing

	// Stop timers
	if r.rotationTimer != nil {
		r.rotationTimer.Stop()
		r.rotationTimer = nil
	}
	if r.durationTimer != nil {
		r.durationTimer.Stop()
		r.durationTimer = nil
	}

	r.mu.Unlock()

	// Stop encoder and finalize file
	r.stopEncoderAndUpload()

	// Stop upload worker
	close(r.uploadStopCh)
	r.uploadWg.Wait()

	r.mu.Lock()
	r.state = StateIdle
	r.uploadStopCh = make(chan struct{}) // Reset for next start
	r.mu.Unlock()

	slog.Info("recorder stopped", "id", r.id, "name", r.config.Name)
	return nil
}

// WriteAudio writes PCM audio to the encoder.
func (r *GenericRecorder) WriteAudio(pcm []byte) error {
	r.mu.RLock()
	state := r.state
	stdin := r.stdin
	r.mu.RUnlock()

	if state != StateRecording || stdin == nil {
		return nil
	}

	n, err := stdin.Write(pcm)
	if err != nil {
		r.mu.Lock()
		r.error = err.Error()
		r.mu.Unlock()
		return err
	}

	r.mu.Lock()
	r.bytesWritten += int64(n)
	r.mu.Unlock()

	return nil
}

// Status returns the current recorder status.
func (r *GenericRecorder) Status() types.RecorderStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	status := types.RecorderStatus{
		State: string(r.state),
		Error: r.error,
	}

	if r.state == StateRecording && !r.startTime.IsZero() {
		status.Duration = time.Since(r.startTime).Seconds()
	}

	return status
}

// IsRecording returns true if currently recording.
func (r *GenericRecorder) IsRecording() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state == StateRecording
}

// UpdateConfig updates the recorder configuration.
// If S3 config changed, the client is recreated.
func (r *GenericRecorder) UpdateConfig(cfg *types.Recorder) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	oldS3 := r.config.S3Bucket + r.config.S3Endpoint + r.config.S3AccessKeyID
	newS3 := cfg.S3Bucket + cfg.S3Endpoint + cfg.S3AccessKeyID

	r.config = *cfg

	// Recreate S3 client if config changed
	if oldS3 != newS3 || cfg.S3SecretAccessKey != "" {
		if r.isS3Configured() {
			client, err := r.createS3Client()
			if err != nil {
				return fmt.Errorf("recreate S3 client: %w", err)
			}
			r.s3Client = client
		} else {
			r.s3Client = nil
		}
	}

	return nil
}

// startEncoderLocked starts the FFmpeg encoder process. Must be called with lock held.
func (r *GenericRecorder) startEncoderLocked() error {
	r.startTime = time.Now()

	// Generate filename based on rotation mode
	var filename string
	if r.config.RotationMode == types.RotationHourly {
		hourStart := truncateToHour(r.startTime)
		filename = r.generateFilename(hourStart)
	} else {
		filename = r.generateFilename(r.startTime)
	}

	recorderDir := filepath.Join(r.tempDir, "recorders", r.id)
	r.currentFile = filepath.Join(recorderDir, filename)

	// Get codec configuration
	codecArgs := r.config.CodecArgs()
	format := r.config.Format()
	ext := r.getFileExtension()

	// Update filename with correct extension
	r.currentFile = r.currentFile[:len(r.currentFile)-len(filepath.Ext(r.currentFile))] + "." + ext

	ctx, cancel := context.WithCancel(context.Background())

	// Build FFmpeg command
	args := []string{
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", SampleRate),
		"-ac", fmt.Sprintf("%d", Channels),
		"-i", "pipe:0",
	}
	args = append(args, "-c:a")
	args = append(args, codecArgs...)
	args = append(args,
		"-f", format,
		"-hide_banner",
		"-loglevel", "warning",
		"-y",
		r.currentFile,
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create stdin pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	r.cmd = cmd
	r.ctx = ctx
	r.cancel = cancel
	r.stdin = stdinPipe
	r.stderr = &stderr
	r.bytesWritten = 0

	if err := cmd.Start(); err != nil {
		cancel()
		if closeErr := stdinPipe.Close(); closeErr != nil {
			slog.Warn("failed to close stdin pipe", "id", r.id, "error", closeErr)
		}
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)
	return nil
}

// stopEncoderAndUpload stops the current encoder and queues the file for upload.
func (r *GenericRecorder) stopEncoderAndUpload() {
	r.mu.Lock()
	stdin := r.stdin
	r.stdin = nil
	currentFile := r.currentFile
	r.mu.Unlock()

	if stdin != nil {
		if err := stdin.Close(); err != nil {
			slog.Warn("failed to close stdin", "id", r.id, "error", err)
		}
	}

	// Wait for FFmpeg to finish
	if r.cmd != nil {
		done := make(chan error, 1)
		go func() {
			done <- r.cmd.Wait()
		}()

		select {
		case err := <-done:
			if err != nil {
				r.mu.Lock()
				if r.stderr != nil {
					errMsg := util.ExtractLastError(r.stderr.String())
					if errMsg != "" {
						r.error = errMsg
					}
				}
				r.mu.Unlock()
			}
		case <-time.After(10 * time.Second):
			slog.Warn("recorder ffmpeg did not stop in time", "id", r.id)
			r.cancel()
		}
	}

	// Queue for upload if file exists and S3 is configured
	if currentFile != "" {
		r.queueForUpload(currentFile)
	}

	if r.statusCallback != nil {
		r.statusCallback()
	}
}

// queueForUpload adds a completed file to the upload queue.
func (r *GenericRecorder) queueForUpload(filePath string) {
	info, err := os.Stat(filePath)
	if err != nil {
		slog.Warn("failed to stat recording file", "id", r.id, "error", err)
		return
	}

	if !r.isS3Configured() {
		slog.Info("S3 not configured, keeping local file", "id", r.id, "path", filePath)
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
	default:
		slog.Warn("upload queue full", "id", r.id)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	r.mu.RLock()
	client := r.s3Client
	bucket := r.config.S3Bucket
	r.mu.RUnlock()

	if client == nil {
		slog.Warn("no S3 client available", "id", r.id)
		return
	}

	contentType := r.getContentType()

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(req.s3Key),
		Body:          file,
		ContentLength: aws.Int64(req.fileSize),
		ContentType:   aws.String(contentType),
	})

	if err != nil {
		r.mu.Lock()
		r.lastUploadErr = err.Error()
		r.mu.Unlock()
		slog.Error("upload failed", "id", r.id, "s3_key", req.s3Key, "error", err)
		return
	}

	now := time.Now()
	r.mu.Lock()
	r.lastUploadTime = &now
	r.lastUploadErr = ""
	r.mu.Unlock()

	// Track for delayed cleanup
	r.cleanupMu.Lock()
	r.uploadedFiles[req.localPath] = now
	r.cleanupMu.Unlock()

	slog.Info("upload completed", "id", r.id, "s3_key", req.s3Key)

	if r.statusCallback != nil {
		r.statusCallback()
	}
}

// cleanupWorker periodically cleans up local files after upload.
func (r *GenericRecorder) cleanupWorker() {
	defer r.uploadWg.Done()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-r.uploadStopCh:
			return
		case <-ticker.C:
			r.cleanupOldFiles()
		}
	}
}

// cleanupOldFiles removes local files that were uploaded more than 1 hour ago.
func (r *GenericRecorder) cleanupOldFiles() {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()

	cutoff := time.Now().Add(-1 * time.Hour)

	for path, uploadTime := range r.uploadedFiles {
		if uploadTime.Before(cutoff) {
			if err := os.Remove(path); err != nil {
				if !os.IsNotExist(err) {
					slog.Warn("failed to cleanup local file", "id", r.id, "path", path, "error", err)
				}
			} else {
				slog.Debug("cleaned up local file", "id", r.id, "path", path)
			}
			delete(r.uploadedFiles, path)
		}
	}
}

// scheduleRotationLocked schedules the next hourly rotation. Must be called with lock held.
// The rotation is staggered by rotationOffset to spread I/O across multiple recorders.
func (r *GenericRecorder) scheduleRotationLocked() {
	duration := timeUntilNextHour(time.Now()) + r.rotationOffset
	r.rotationTimer = time.AfterFunc(duration, r.rotateFile)
}

// rotateFile handles hourly file rotation.
func (r *GenericRecorder) rotateFile() {
	r.mu.Lock()

	if r.state != StateRecording {
		r.mu.Unlock()
		return
	}

	slog.Info("recorder rotating file at hour boundary", "id", r.id)
	r.mu.Unlock()

	// Stop current encoder and upload
	r.stopEncoderAndUpload()

	r.mu.Lock()
	// Start new encoder
	if err := r.startEncoderLocked(); err != nil {
		slog.Error("failed to start new recording file", "id", r.id, "error", err)
		r.error = err.Error()
	}

	// Schedule next rotation
	r.scheduleRotationLocked()
	r.mu.Unlock()

	if r.statusCallback != nil {
		r.statusCallback()
	}
}

// scheduleDurationLimitLocked schedules auto-stop for on-demand mode. Must be called with lock held.
func (r *GenericRecorder) scheduleDurationLimitLocked() {
	duration := time.Duration(r.maxDurationMinutes) * time.Minute
	r.durationTimer = time.AfterFunc(duration, func() {
		slog.Info("recorder max duration reached", "id", r.id, "duration", duration)
		if err := r.Stop(); err != nil {
			slog.Error("failed to stop recorder after max duration", "id", r.id, "error", err)
		}
	})
}

// generateFilename creates a filename for the given timestamp.
func (r *GenericRecorder) generateFilename(t time.Time) string {
	ext := r.getFileExtension()
	safeName := sanitizeFilename(r.config.Name)
	return fmt.Sprintf("%s-%s.%s", safeName, t.Format("2006-01-02-15-04"), ext)
}

// generateS3Key creates the S3 object key.
func (r *GenericRecorder) generateS3Key(filename string) string {
	// Flat path with prefix: recordings/recorder-name-2025-01-15-14-00.mp3
	return fmt.Sprintf("recordings/%s", filename)
}

// getFileExtension returns the file extension for the configured codec.
func (r *GenericRecorder) getFileExtension() string {
	switch r.config.Codec {
	case "mp2":
		return "mp2"
	case "mp3":
		return "mp3"
	case "ogg":
		return "ogg"
	case "wav":
		return "mkv" // WAV uses matroska container
	default:
		return "mp3"
	}
}

// getContentType returns the MIME type for the configured codec.
func (r *GenericRecorder) getContentType() string {
	switch r.config.Codec {
	case "mp2":
		return "audio/mpeg"
	case "mp3":
		return "audio/mpeg"
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/x-matroska"
	default:
		return "audio/mpeg"
	}
}

// isS3Configured returns true if S3 is configured for this recorder.
func (r *GenericRecorder) isS3Configured() bool {
	return r.config.S3Bucket != "" && r.config.S3AccessKeyID != "" && r.config.S3SecretAccessKey != ""
}

// createS3Client creates an S3 client for this recorder.
func (r *GenericRecorder) createS3Client() (*s3.Client, error) {
	cfg := &S3Config{
		Endpoint:        r.config.S3Endpoint,
		Bucket:          r.config.S3Bucket,
		AccessKeyID:     r.config.S3AccessKeyID,
		SecretAccessKey: r.config.S3SecretAccessKey,
	}
	return createS3Client(cfg)
}

// TestS3 tests the S3 connection for this recorder.
func (r *GenericRecorder) TestS3() error {
	cfg := &S3Config{
		Endpoint:        r.config.S3Endpoint,
		Bucket:          r.config.S3Bucket,
		AccessKeyID:     r.config.S3AccessKeyID,
		SecretAccessKey: r.config.S3SecretAccessKey,
	}
	return TestS3Connection(cfg)
}

// sanitizeFilename removes or replaces characters that are invalid in filenames.
func sanitizeFilename(name string) string {
	result := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result = append(result, c)
		} else if c == ' ' {
			result = append(result, '-')
		}
	}
	if len(result) == 0 {
		return "recording"
	}
	return string(result)
}

// TestRecorderS3Connection tests S3 connectivity for a recorder configuration.
func TestRecorderS3Connection(cfg *types.Recorder) error {
	s3Cfg := &S3Config{
		Endpoint:        cfg.S3Endpoint,
		Bucket:          cfg.S3Bucket,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
	}
	return TestS3Connection(s3Cfg)
}
