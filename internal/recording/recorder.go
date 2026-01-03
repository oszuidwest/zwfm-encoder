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

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GenericRecorder handles recording to a file with optional S3 upload.
type GenericRecorder struct {
	mu sync.RWMutex // Protects state, config, file paths

	id                 string
	config             types.Recorder
	ffmpegPath         string
	maxDurationMinutes int // For on-demand mode (from global config)

	tempDir   string
	state     types.ProcessState
	lastError string

	// FFmpeg process
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser
	stderr *bytes.Buffer

	// stdinMu protects stdin I/O from race conditions.
	// WriteAudio and stopEncoderAndUpload both need access to stdin.
	// Without this, Close() could happen while Write() is in progress.
	stdinMu sync.Mutex

	// Current recording
	currentFile string
	startTime   time.Time

	// S3 client
	s3Client *s3.Client

	// Upload queue
	uploadQueue  chan uploadRequest
	uploadWg     sync.WaitGroup
	uploadStopCh chan struct{}
	stopOnce     sync.Once // Prevents double-close of uploadStopCh

	// Rotation timer (hourly mode)
	rotationTimer *time.Timer

	// Max duration timer (on-demand mode)
	durationTimer *time.Timer
}

// NewGenericRecorder creates a new recorder with the given configuration.
func NewGenericRecorder(cfg *types.Recorder, ffmpegPath, tempDir string, maxDurationMinutes int) (*GenericRecorder, error) {
	r := &GenericRecorder{
		id:                 cfg.ID,
		config:             *cfg,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		tempDir:            tempDir,
		state:              types.ProcessStopped,
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

// Config returns the recorder configuration.
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

// IsCurrentFile reports whether the given path is the currently recording file.
func (r *GenericRecorder) IsCurrentFile(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentFile == path
}

// Start begins recording.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == types.ProcessRunning || r.state == types.ProcessStarting {
		return ErrAlreadyRecording
	}

	// Clear any previous error and set starting state
	r.lastError = ""
	r.state = types.ProcessStarting

	// Start async validation and startup
	go r.startAsync()

	return nil
}

// startAsync performs path validation and starts the FFmpeg encoder.
// Called as a goroutine from Start().
func (r *GenericRecorder) startAsync() {
	// Read config values we need for validation
	r.mu.RLock()
	storageMode := r.config.StorageMode
	localPath := r.config.LocalPath
	id := r.id
	tempDir := r.tempDir
	r.mu.RUnlock()

	// Validate and prepare output directory based on storage mode
	if storageMode == types.StorageS3 {
		// S3-only: create temp directory (should always be writable)
		if err := os.MkdirAll(filepath.Join(tempDir, "recorders", id), 0o755); err != nil {
			r.setError(fmt.Sprintf("failed to create temp directory: %v", err))
			return
		}
	} else {
		// Local or Both: validate path and check writability
		// CheckPathWritable also creates the directory if needed
		if err := util.ValidatePath("local_path", localPath); err != nil {
			r.setError(fmt.Sprintf("invalid local_path: %v", err))
			return
		}
		if err := util.CheckPathWritable(localPath); err != nil {
			r.setError("local path is not writable")
			return
		}
	}

	// Now lock and start the encoder
	r.mu.Lock()

	// Re-check state in case Stop() was called during validation
	if r.state != types.ProcessStarting {
		r.mu.Unlock()
		return
	}

	// Start FFmpeg encoder
	if err := r.startEncoderLocked(); err != nil {
		r.state = types.ProcessError
		r.lastError = err.Error()
		r.mu.Unlock()
		slog.Error("recorder failed to start encoder", "id", r.id, "error", err)
		return
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

	r.state = types.ProcessRunning

	slog.Info("recorder started", "id", r.id, "name", r.config.Name, "mode", r.config.RotationMode)
	r.mu.Unlock()
}

// setError transitions the recorder to error state with a message.
func (r *GenericRecorder) setError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = types.ProcessError
	r.lastError = msg
	slog.Error("recorder error", "id", r.id, "error", msg)
}

// Stop ends recording.
func (r *GenericRecorder) Stop() error {
	r.mu.Lock()

	if r.state == types.ProcessStopped || r.state == types.ProcessStopping {
		r.mu.Unlock()
		return nil
	}

	// If in error or starting state, just reset to stopped
	if r.state == types.ProcessError || r.state == types.ProcessStarting {
		r.state = types.ProcessStopped
		r.lastError = ""
		r.mu.Unlock()
		return nil
	}

	// ProcessRunning or ProcessRotating - proceed with full stop sequence
	r.state = types.ProcessStopping

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

	// Stop upload worker - use Once to prevent double-close panic
	r.stopOnce.Do(func() {
		close(r.uploadStopCh)
	})
	r.uploadWg.Wait()

	r.mu.Lock()
	r.state = types.ProcessStopped
	r.uploadStopCh = make(chan struct{})          // Reset for next start
	r.uploadQueue = make(chan uploadRequest, 100) // Reset for next start
	r.stopOnce = sync.Once{}                      // Reset Once for next start
	r.mu.Unlock()

	slog.Info("recorder stopped", "id", r.id, "name", r.config.Name)
	return nil
}

// WriteAudio writes PCM audio data.
func (r *GenericRecorder) WriteAudio(pcm []byte) error {
	r.mu.RLock()
	state := r.state
	r.mu.RUnlock()

	// Accept both Running and Rotating - rotation closes stdin when ready
	if state != types.ProcessRunning && state != types.ProcessRotating {
		return nil
	}

	// Hold stdinMu during write to prevent race with close
	r.stdinMu.Lock()
	stdin := r.stdin
	if stdin == nil {
		r.stdinMu.Unlock()
		return nil
	}
	_, err := stdin.Write(pcm)
	r.stdinMu.Unlock()

	if err != nil {
		r.mu.Lock()
		r.lastError = err.Error()
		r.mu.Unlock()
		return err
	}

	return nil
}

// Status returns the current recorder status.
func (r *GenericRecorder) Status() types.ProcessStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return types.ProcessStatus{
		State: r.state,
		Error: r.lastError,
	}
}

// IsRecording reports whether recording is currently in progress.
func (r *GenericRecorder) IsRecording() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state == types.ProcessRunning
}

// UpdateConfig updates the recorder configuration.
func (r *GenericRecorder) UpdateConfig(cfg *types.Recorder) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Clear error state on config update (user may have fixed the issue)
	if r.state == types.ProcessError {
		r.state = types.ProcessStopped
		r.lastError = ""
	}

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

	// Build FFmpeg command args
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-c:a")
	args = append(args, codecArgs...)
	args = append(args,
		"-f", format,
		"-hide_banner",
		"-loglevel", "warning",
		"-y",
		r.currentFile,
	)

	// Start FFmpeg process
	proc, err := ffmpeg.StartProcess(r.ffmpegPath, args)
	if err != nil {
		return err
	}

	r.cmd = proc.Cmd
	r.cancel = proc.Cancel
	r.stdin = proc.Stdin
	r.stderr = proc.Stderr

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)
	return nil
}

// stopEncoderAndUpload stops the current encoder and queues the file for upload.
func (r *GenericRecorder) stopEncoderAndUpload() {
	// Cache values under lock to prevent race conditions
	r.mu.Lock()
	currentFile := r.currentFile
	cmd := r.cmd
	cancel := r.cancel
	stderr := r.stderr
	r.mu.Unlock()

	// Close stdin under stdinMu to prevent race with WriteAudio
	r.stdinMu.Lock()
	stdin := r.stdin
	r.stdin = nil
	r.stdinMu.Unlock()

	if stdin != nil {
		if err := stdin.Close(); err != nil {
			slog.Warn("failed to close stdin", "id", r.id, "error", err)
		}
	}

	// Wait for FFmpeg to finish with 2-stage timeout
	if cmd == nil {
		slog.Error("stopEncoderAndUpload: cmd is nil", "id", r.id)
		return
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			r.mu.Lock()
			if stderr != nil {
				errMsg := util.ExtractLastError(stderr.String())
				if errMsg != "" {
					r.lastError = errMsg
				}
			}
			r.mu.Unlock()
		}
	case <-time.After(10000 * time.Millisecond):
		// Stage 1: Cancel context (sends SIGTERM)
		slog.Warn("recorder ffmpeg did not stop in time, canceling context", "id", r.id)
		if cancel != nil {
			cancel()
		}

		// Stage 2: Wait for graceful shutdown or force kill
		select {
		case <-done:
			// Process stopped after cancel
		case <-time.After(2000 * time.Millisecond):
			// Force kill if still running
			if cmd.Process != nil {
				slog.Error("recorder ffmpeg force killed", "id", r.id)
				_ = cmd.Process.Kill()
				<-done // Wait for process to exit after kill
			}
		}
	}

	// Queue for upload if file exists and S3 is configured
	if currentFile != "" {
		r.queueForUpload(currentFile)
	}
}

// scheduleRotationLocked schedules the next hourly rotation. Must be called with lock held.
func (r *GenericRecorder) scheduleRotationLocked() {
	duration := timeUntilNextHour(time.Now())
	r.rotationTimer = time.AfterFunc(duration, r.rotateFile)
}

// rotateFile handles hourly file rotation.
func (r *GenericRecorder) rotateFile() {
	r.mu.Lock()

	if r.state != types.ProcessRunning {
		r.mu.Unlock()
		return
	}

	// Set rotating state before releasing lock to prevent Stop() interference
	r.state = types.ProcessRotating
	slog.Info("recorder rotating file at hour boundary", "id", r.id)
	r.mu.Unlock()

	// Stop current encoder and upload
	r.stopEncoderAndUpload()

	r.mu.Lock()
	// Re-check state after reacquiring lock - Stop() may have been called
	if r.state != types.ProcessRotating {
		// State changed (likely Stop() was called) - don't continue rotation
		r.mu.Unlock()
		return
	}

	// Start new encoder
	if err := r.startEncoderLocked(); err != nil {
		slog.Error("failed to start new recording file after rotation", "id", r.id, "error", err)
		r.state = types.ProcessError
		r.lastError = err.Error()
		r.mu.Unlock()
		return // Don't schedule next rotation - hourly retry will pick this up
	}

	// Schedule next rotation
	r.scheduleRotationLocked()
	r.state = types.ProcessRunning
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
	case types.CodecMP2:
		return "mp2"
	case types.CodecMP3:
		return "mp3"
	case types.CodecOGG:
		return "ogg"
	case types.CodecWAV:
		return "mkv" // WAV uses matroska container
	default:
		return "mp3"
	}
}

// getContentType returns the MIME type for the configured codec.
func (r *GenericRecorder) getContentType() string {
	switch r.config.Codec {
	case types.CodecMP2:
		return "audio/mpeg"
	case types.CodecMP3:
		return "audio/mpeg"
	case types.CodecOGG:
		return "audio/ogg"
	case types.CodecWAV:
		return "audio/x-matroska"
	default:
		return "audio/mpeg"
	}
}

// isS3Configured reports whether S3 is configured for this recorder.
func (r *GenericRecorder) isS3Configured() bool {
	return r.config.S3Bucket != "" && r.config.S3AccessKeyID != "" && r.config.S3SecretAccessKey != ""
}

// createS3Client creates an S3 client for this recorder.
func (r *GenericRecorder) createS3Client() (*s3.Client, error) {
	return createS3Client(RecorderToS3Config(&r.config))
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
