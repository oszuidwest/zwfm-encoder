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
	maxDurationMinutes int // For on-demand mode (from global config)

	tempDir   string
	state     RecordingState
	lastError string

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
}

// uploadRequest represents a file ready for S3 upload.
type uploadRequest struct {
	localPath string
	s3Key     string
	fileSize  int64
}

// NewGenericRecorder creates a new recorder with the given configuration.
// maxDurationMinutes is the global max duration for on-demand recorders.
func NewGenericRecorder(cfg *types.Recorder, tempDir string, maxDurationMinutes int) (*GenericRecorder, error) {
	r := &GenericRecorder{
		id:                 cfg.ID,
		config:             *cfg,
		maxDurationMinutes: maxDurationMinutes,
		tempDir:            tempDir,
		state:              StateIdle,
		uploadQueue:        make(chan uploadRequest, 100),
		uploadStopCh:       make(chan struct{}),
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

// S3Client returns the S3 client for this recorder.
func (r *GenericRecorder) S3Client() *s3.Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.s3Client
}

// IsCurrentFile returns true if the given path is the currently recording file.
func (r *GenericRecorder) IsCurrentFile(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentFile == path
}

// Start begins recording.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == StateRecording {
		return ErrAlreadyRecording
	}

	// Create output directory based on storage mode
	var outputDir string
	if r.config.StorageMode == types.StorageS3 {
		// S3-only: use temp directory
		outputDir = filepath.Join(r.tempDir, "recorders", r.id)
	} else {
		// Local or Both: use configured LocalPath
		outputDir = r.config.LocalPath
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Start FFmpeg encoder
	if err := r.startEncoderLocked(); err != nil {
		return err
	}

	// Start upload worker (after encoder to prevent goroutine leak on failure)
	r.uploadWg.Add(1)
	go r.uploadWorker()

	// Schedule based on rotation mode
	if r.config.RotationMode == types.RotationHourly {
		r.scheduleRotationLocked()
	} else if r.maxDurationMinutes > 0 {
		r.scheduleDurationLimitLocked()
	}

	r.state = StateRecording
	r.lastError = ""

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
	r.uploadStopCh = make(chan struct{})          // Reset for next start
	r.uploadQueue = make(chan uploadRequest, 100) // Reset for next start
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
		r.lastError = err.Error()
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
		Error: r.lastError,
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

	// Determine output directory based on storage mode
	var outputDir string
	if r.config.StorageMode == types.StorageS3 {
		// S3-only: use temp directory
		outputDir = filepath.Join(r.tempDir, "recorders", r.id)
	} else {
		// Local or Both: use configured LocalPath
		outputDir = r.config.LocalPath
	}
	r.currentFile = filepath.Join(outputDir, filename)

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

	// Wait for FFmpeg to finish with 2-stage timeout
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
						r.lastError = errMsg
					}
				}
				r.mu.Unlock()
			}
		case <-time.After(10 * time.Second):
			// Stage 1: Cancel context (sends SIGTERM)
			slog.Warn("recorder ffmpeg did not stop in time, canceling context", "id", r.id)
			r.cancel()

			// Stage 2: Wait for graceful shutdown or force kill
			select {
			case <-done:
				// Process stopped after cancel
			case <-time.After(2 * time.Second):
				// Force kill if still running
				if r.cmd.Process != nil {
					slog.Error("recorder ffmpeg force killed", "id", r.id)
					_ = r.cmd.Process.Kill()
					<-done // Wait for process to exit after kill
				}
			}
		}
	}

	// Queue for upload if file exists and S3 is configured
	if currentFile != "" {
		r.queueForUpload(currentFile)
	}
}

// queueForUpload adds a completed file to the upload queue based on storage mode.
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
	storageMode := r.config.StorageMode
	r.mu.Unlock()

	slog.Info("upload completed", "id", r.id, "s3_key", req.s3Key)

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

// scheduleRotationLocked schedules the next hourly rotation. Must be called with lock held.
func (r *GenericRecorder) scheduleRotationLocked() {
	duration := timeUntilNextHour(time.Now())
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
		r.lastError = err.Error()
	}

	// Schedule next rotation
	r.scheduleRotationLocked()
	r.mu.Unlock()
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

// generateS3Key creates the S3 object key with recorder-specific prefix.
func (r *GenericRecorder) generateS3Key(filename string) string {
	// Prefix: recordings/{sanitized-recorder-name}/filename
	safeName := sanitizeFilename(r.config.Name)
	return fmt.Sprintf("recordings/%s/%s", safeName, filename)
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
	return createS3Client(RecorderToS3Config(&r.config))
}

// TestS3 tests the S3 connection for this recorder.
func (r *GenericRecorder) TestS3() error {
	return TestS3Connection(RecorderToS3Config(&r.config))
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
	return TestS3Connection(RecorderToS3Config(cfg))
}
