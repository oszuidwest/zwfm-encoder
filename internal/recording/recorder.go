package recording

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GenericRecorder is a recorder that saves audio to files with optional S3 upload.
type GenericRecorder struct {
	mu sync.RWMutex // Protects state, config, file paths

	id                 string
	config             types.Recorder
	ffmpegPath         string
	maxDurationMinutes int // For on-demand mode (from global config)
	eventLogger        *eventlog.Logger

	tempDir   string
	state     types.ProcessState
	lastError string

	// FFmpeg process (encapsulates cmd, stdin, stderr with thread-safe access)
	result *ffmpeg.StartResult

	// Current recording
	currentFile string
	startTime   time.Time

	// S3 client (cached, recreated when config changes)
	s3Client    *s3.Client
	s3ConfigKey string // Config key used to create cached client

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
// S3 client is created lazily on first use (same pattern as Graph client).
func NewGenericRecorder(cfg *types.Recorder, ffmpegPath, tempDir string, maxDurationMinutes int, eventLogger *eventlog.Logger) (*GenericRecorder, error) {
	r := &GenericRecorder{
		id:                 cfg.ID,
		config:             *cfg,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		eventLogger:        eventLogger,
		tempDir:            tempDir,
		state:              types.ProcessStopped,
		uploadQueue:        make(chan uploadRequest, 100),
		uploadStopCh:       make(chan struct{}),
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

// s3ConfigKeyFrom returns a string key for comparing S3 configurations.
// Used to detect when credentials change and client needs recreation.
// Includes all fields baked into the S3 client at creation time:
// - Endpoint, AccessKeyID, SecretAccessKey: client configuration
// Bucket is NOT included as it's passed per-operation, not stored in client.
func s3ConfigKeyFrom(cfg *types.Recorder) string {
	return cfg.S3Endpoint + "|" + cfg.S3AccessKeyID + "|" + cfg.S3SecretAccessKey
}

// getOrCreateS3Client returns the cached S3 client, recreating if config changed.
// This follows the same pattern as Graph client handling in notifications.
func (r *GenericRecorder) getOrCreateS3Client() (*s3.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isS3Configured() {
		return nil, nil
	}

	newKey := s3ConfigKeyFrom(&r.config)

	// Reuse cached client if config unchanged
	if r.s3Client != nil && r.s3ConfigKey == newKey {
		return r.s3Client, nil
	}

	// Recreate client with new config
	client, err := r.createS3Client()
	if err != nil {
		return nil, err
	}
	r.s3Client = client
	r.s3ConfigKey = newKey
	return client, nil
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

// startAsync validates paths and starts the encoder asynchronously.
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

	// Log started event (need to unlock first since logEvent reads config)
	r.mu.Unlock()
	r.logEvent(eventlog.RecorderStarted, "", "", 0, 0, "")
}

// setError sets the recorder to error state with the given message.
func (r *GenericRecorder) setError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = types.ProcessError
	r.lastError = msg
	slog.Error("recorder error", "id", r.id, "error", msg)

	// Log to event log
	r.logEvent(eventlog.RecorderError, "", msg, 0, 0, "")
}

// logEvent logs a recorder event to the event log.
func (r *GenericRecorder) logEvent(eventType eventlog.EventType, filename, errMsg string, retryCount, filesDeleted int, storageType string) {
	if r.eventLogger == nil {
		return
	}
	_ = r.eventLogger.LogRecorder(
		eventType,
		r.config.Name,
		filename,
		string(r.config.Codec),
		string(r.config.StorageMode),
		"", // s3Key - set separately for upload events
		errMsg,
		retryCount,
		filesDeleted,
		storageType,
	)
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
	r.logEvent(eventlog.RecorderStopped, "", "", 0, 0, "")
	return nil
}

// WriteAudio writes PCM audio data.
func (r *GenericRecorder) WriteAudio(pcm []byte) error {
	r.mu.RLock()
	state := r.state
	result := r.result
	r.mu.RUnlock()

	// Accept both Running and Rotating - rotation closes stdin when ready
	if state != types.ProcessRunning && state != types.ProcessRotating {
		return nil
	}

	if result == nil {
		return nil
	}

	// WriteStdin is thread-safe (mutex encapsulated in StartResult)
	_, err := result.WriteStdin(pcm)
	if err != nil {
		// ErrStdinClosed is expected during rotation/shutdown - not a real error
		if errors.Is(err, ffmpeg.ErrStdinClosed) {
			return nil
		}

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
// S3 client recreation is handled lazily by getOrCreateS3Client on next use.
func (r *GenericRecorder) UpdateConfig(cfg *types.Recorder) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Clear error state on config update (user may have fixed the issue)
	if r.state == types.ProcessError {
		r.state = types.ProcessStopped
		r.lastError = ""
	}

	r.config = *cfg
	// Note: S3 client will be recreated on next use if config changed
	// (same pattern as Graph client in notifications)

	return nil
}

// startEncoderLocked starts the FFmpeg encoder process.
func (r *GenericRecorder) startEncoderLocked() error {
	r.startTime = time.Now()

	// Generate filename based on rotation mode
	var filename string
	if r.config.RotationMode == types.RotationHourly {
		hourStart := r.startTime.Truncate(time.Hour)
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
	result, err := ffmpeg.StartProcess(r.ffmpegPath, args)
	if err != nil {
		return err
	}

	r.result = result

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)

	// Log new file event (eventLogger is thread-safe)
	if r.eventLogger != nil {
		_ = r.eventLogger.LogRecorder(
			eventlog.RecorderFile,
			r.config.Name,
			filepath.Base(r.currentFile),
			string(r.config.Codec),
			string(r.config.StorageMode),
			"", "", 0, 0, "",
		)
	}
	return nil
}

// stopEncoderAndUpload stops the current encoder and handles the recorded file.
func (r *GenericRecorder) stopEncoderAndUpload() {
	// Cache values under lock to prevent race conditions
	r.mu.Lock()
	currentFile := r.currentFile
	result := r.result
	r.mu.Unlock()

	if result == nil {
		slog.Error("stopEncoderAndUpload: result is nil", "id", r.id)
		return
	}

	// Close stdin - signals FFmpeg that input is done
	result.CloseStdin()

	// Wait for FFmpeg to finish with timeout
	done := make(chan error, 1)
	go func() {
		done <- result.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			r.mu.Lock()
			errMsg := util.ExtractLastError(result.Stderr())
			if errMsg != "" {
				r.lastError = errMsg
			}
			r.mu.Unlock()
		}
	case <-time.After(10 * time.Second):
		// Stage 1: Send SIGTERM for graceful shutdown
		slog.Warn("recorder ffmpeg did not stop in time, sending signal", "id", r.id)
		_ = result.Signal()

		// Stage 2: Wait for graceful shutdown or force kill
		select {
		case <-done:
			// Process stopped after signal
		case <-time.After(2 * time.Second):
			// Force kill if still running
			slog.Error("recorder ffmpeg force killed", "id", r.id)
			_ = result.Kill()
			<-done // Wait for process to exit after kill
		}
	}

	// Queue for upload if file exists and S3 is configured
	if currentFile != "" {
		r.queueForUpload(currentFile)
	}
}

// scheduleRotationLocked schedules the next hourly rotation.
func (r *GenericRecorder) scheduleRotationLocked() {
	duration := util.TimeUntilNextHour(time.Now())
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

// scheduleDurationLimitLocked schedules auto-stop for on-demand recorders.
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

// generateS3Key creates the S3 object key for a recording file.
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

// sanitizeFilename returns a safe filename from the given name.
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
