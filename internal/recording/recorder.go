package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// errRecorderStopped marks intentional stop/rotation cancellation.
var errRecorderStopped = errors.New("recorder stopped")

const (
	// recorderAudioBufferChunks bounds backlog to about 3s of distributor audio.
	recorderAudioBufferChunks = 30
	// writerDrainTimeout bounds graceful writer drain before cancelling FFmpeg.
	writerDrainTimeout = 5 * time.Second
	// processStopTimeout bounds waiting before escalating to SIGTERM.
	processStopTimeout = 10 * time.Second
	// processKillTimeout bounds waiting after SIGTERM before SIGKILL.
	processKillTimeout = 2 * time.Second
)

// GenericRecorder saves audio to files with optional S3 upload.
type GenericRecorder struct {
	mu sync.RWMutex // Protects state, config, file paths

	id                 string
	config             types.Recorder
	ffmpegPath         string
	maxDurationMinutes int // For on-demand mode (from global config)
	eventLogger        *eventlog.Logger
	onUploadAbandoned  UploadAbandonedCallback

	spoolDir  string
	state     types.ProcessState
	lastError string

	// FFmpeg process (encapsulates cmd, stdin, stderr with thread-safe access)
	result *ffmpeg.StartResult

	// Per-process writer state; reset with each FFmpeg process.
	audioCh    chan []byte
	writerDone chan struct{}
	audioDrops atomic.Int64 // Lost chunks from overflow or teardown.

	// Current recording
	currentFile string
	startTime   time.Time

	// S3 client (cached, recreated when config changes)
	s3Client    *s3.Client
	s3ConfigKey string // Config key used to create cached client

	// Upload queue
	uploadQueue         chan uploadRequest
	uploadWg            sync.WaitGroup
	uploadStopCh        chan struct{}
	stopOnce            sync.Once // Prevents double-close of uploadStopCh
	uploadWorkerRunning bool      // Guards against starting multiple upload workers
	uploadClosed        bool      // Routes late uploads to retry after Stop.

	// Retry queue for failed uploads (protected by mu)
	retryQueue []pendingUpload

	// Rotation timer (hourly mode)
	rotationTimer *time.Timer

	// Max duration timer (on-demand mode)
	durationTimer *time.Timer
}

// GenericRecorderConfig holds the parameters for creating a GenericRecorder.
type GenericRecorderConfig struct {
	Recorder           *types.Recorder
	FFmpegPath         string
	SpoolDir           string
	MaxDurationMinutes int
	EventLogger        *eventlog.Logger
	OnUploadAbandoned  UploadAbandonedCallback
}

// NewGenericRecorder creates a new recorder instance.
func NewGenericRecorder(cfg GenericRecorderConfig) (*GenericRecorder, error) {
	r := &GenericRecorder{
		id:                 cfg.Recorder.ID,
		config:             *cfg.Recorder,
		ffmpegPath:         cfg.FFmpegPath,
		maxDurationMinutes: cfg.MaxDurationMinutes,
		eventLogger:        cfg.EventLogger,
		onUploadAbandoned:  cfg.OnUploadAbandoned,
		spoolDir:           cfg.SpoolDir,
		state:              types.ProcessStopped,
		uploadQueue:        make(chan uploadRequest, 100),
		uploadStopCh:       make(chan struct{}),
	}

	return r, nil
}

// ID returns the recorder ID.
func (r *GenericRecorder) ID() string {
	return r.id
}

// Config returns a copy of the recorder configuration.
func (r *GenericRecorder) Config() types.Recorder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config
}

// SetMaxDurationMinutes updates the per-recorder duration limit from global config.
// Affects new recordings only; a recording already in progress is not interrupted.
func (r *GenericRecorder) SetMaxDurationMinutes(minutes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxDurationMinutes = minutes
}

func s3ConfigKeyFrom(cfg *types.Recorder) string {
	return cfg.S3Endpoint + "|" + cfg.S3AccessKeyID + "|" + cfg.S3SecretAccessKey
}

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

// Start starts the recorder.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == types.ProcessRunning || r.state == types.ProcessStarting {
		return ErrAlreadyRecording
	}

	// Clear any previous error and set starting state
	r.lastError = ""
	r.audioDrops.Store(0)
	r.state = types.ProcessStarting

	// Start async validation and startup
	go r.startAsync()

	return nil
}

func (r *GenericRecorder) startAsync() {
	// Read config values we need for validation
	r.mu.RLock()
	storageMode := r.config.StorageMode
	localPath := r.config.LocalPath
	id := r.id
	spoolDir := r.spoolDir
	r.mu.RUnlock()

	// Validate and prepare output directory based on storage mode
	if storageMode == types.StorageS3 {
		// S3-only: create spool directory (should always be writable)
		//nolint:gosec // Spool directory needs to be readable
		if err := os.MkdirAll(filepath.Join(spoolDir, "recorders", id), 0o755); err != nil {
			r.setError(fmt.Sprintf("failed to create spool directory: %v", err))
			return
		}
	}

	if storageMode != types.StorageS3 {
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
	if !r.uploadWorkerRunning {
		r.uploadWorkerRunning = true
		r.uploadClosed = false // Reopen upload intake.
		r.uploadWg.Add(1)
		go r.uploadWorker()
	}

	// Schedule based on recording mode
	if r.config.RecordingMode == types.RecordingHourly {
		r.scheduleRotationLocked()
	} else if r.maxDurationMinutes > 0 {
		r.scheduleDurationLimitLocked()
	}

	r.state = types.ProcessRunning

	// Capture log params while holding lock
	logParams := r.captureLogParamsLocked()
	name := r.config.Name
	mode := r.config.RecordingMode
	r.mu.Unlock()

	slog.Info("recorder started", "id", r.id, "name", name, "mode", mode)
	r.logEvent(eventlog.RecorderStarted, logParams)
}

func (r *GenericRecorder) setError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = types.ProcessError
	r.lastError = msg
	slog.Error("recorder error", "id", r.id, "error", msg)

	// Log to event log (capture params while holding lock)
	p := r.captureLogParamsLocked()
	p.Error = msg
	r.logEvent(eventlog.RecorderError, p)
}

// Must be called with r.mu held.
func (r *GenericRecorder) captureLogParamsLocked() *eventlog.RecorderEventParams {
	return &eventlog.RecorderEventParams{
		RecorderName: r.config.Name,
		Codec:        string(r.config.Codec),
		StorageMode:  string(r.config.StorageMode),
	}
}

func (r *GenericRecorder) logEvent(eventType eventlog.EventType, p *eventlog.RecorderEventParams) {
	if r.eventLogger == nil {
		return
	}
	if err := r.eventLogger.LogRecorder(eventType, p); err != nil {
		slog.Warn("failed to log recorder event", "type", eventType, "error", err)
	}
}

// Stop stops the recorder.
func (r *GenericRecorder) Stop() error {
	r.mu.Lock()

	// Already stopped or stopping
	if r.state == types.ProcessStopped || r.state == types.ProcessStopping {
		r.mu.Unlock()
		return nil
	}

	r.state = types.ProcessStopping

	// Always stop timers (may be running even after write error)
	if r.rotationTimer != nil {
		r.rotationTimer.Stop()
		r.rotationTimer = nil
	}
	if r.durationTimer != nil {
		r.durationTimer.Stop()
		r.durationTimer = nil
	}
	r.mu.Unlock()

	// Queue the final file while the upload worker can still drain.
	r.stopEncoderAndUpload()

	// Late rotation uploads must persist to the retry queue after this point.
	r.mu.Lock()
	r.uploadClosed = true
	r.mu.Unlock()

	// Stop the upload worker even after write errors.
	r.stopOnce.Do(func() {
		close(r.uploadStopCh)
	})
	r.uploadWg.Wait()

	r.mu.Lock()
	r.state = types.ProcessStopped
	r.lastError = ""
	r.uploadStopCh = make(chan struct{}) // Fresh stop channel for next Start.
	// Reuse uploadQueue; uploadClosed routes late files to the retry queue.
	r.stopOnce = sync.Once{}      // Allow the next Stop to close uploadStopCh.
	r.uploadWorkerRunning = false // Allow the next Start to launch a worker.

	// Capture log params while holding lock
	logParams := r.captureLogParamsLocked()
	name := r.config.Name
	r.mu.Unlock()

	slog.Info("recorder stopped", "id", r.id, "name", name)
	r.logEvent(eventlog.RecorderStopped, logParams)
	return nil
}

// WriteAudio queues pcm for non-blocking FFmpeg stdin writes.
// The caller must keep pcm immutable after the call.
// On full backlog, WriteAudio drops the newest chunk and increments AudioDrops.
func (r *GenericRecorder) WriteAudio(pcm []byte) {
	// Keep the send under r.mu so teardown cannot close ch between check and send.
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Accept a live rotating writer; IsRecording and WriteAudio are not atomic.
	ch := r.audioCh
	if ch == nil || (r.state != types.ProcessRunning && r.state != types.ProcessRotating) {
		return
	}

	select {
	case ch <- pcm:
	default:
		drops := r.audioDrops.Add(1)
		if drops == 1 || drops%100 == 0 {
			slog.Warn("recorder audio buffer full, dropping chunk; recording will have a gap",
				"id", r.id, "total_drops", drops)
		}
	}
}

// audioWriter owns blocking stdin writes for one FFmpeg process.
func (r *GenericRecorder) audioWriter(result *ffmpeg.StartResult, audioCh <-chan []byte, writerDone chan<- struct{}) {
	defer close(writerDone)

	for pcm := range audioCh {
		_, err := result.WriteStdin(pcm)
		if err == nil {
			continue
		}

		// Stop and rotation intentionally close stdin or cancel the process.
		if errors.Is(err, ffmpeg.ErrStdinClosed) ||
			errors.Is(context.Cause(result.Context()), errRecorderStopped) {
			return
		}

		// Unexpected write errors mean FFmpeg crashed or exited on its own.
		r.handleWriteError(result, err)
		return
	}
}

// handleWriteError finalizes the partial file after an unexpected write failure.
func (r *GenericRecorder) handleWriteError(result *ffmpeg.StartResult, writeErr error) {
	r.mu.Lock()
	// Rotation or Stop owns cleanup for swapped processes.
	if r.result != result {
		r.mu.Unlock()
		return
	}
	r.state = types.ProcessError
	r.lastError = writeErr.Error()
	capturedFile := r.currentFile
	r.result = nil
	r.currentFile = ""
	r.audioCh = nil
	// audioWriter closes writerDone when it returns.
	r.writerDone = nil
	r.mu.Unlock()

	slog.Warn("recorder write failed, finalizing recording", "id", r.id, "error", writeErr)

	// Let audioWriter return before cleanup waits on FFmpeg.
	go r.cleanupAfterWriteError(result, capturedFile)
}

// cleanupAfterWriteError closes a broken FFmpeg process and uploads its partial file.
// The failing write has released stdinMu, so CloseStdin cannot deadlock.
func (r *GenericRecorder) cleanupAfterWriteError(result *ffmpeg.StartResult, currentFile string) {
	if result == nil {
		return
	}

	slog.Warn("recorder write error, cleaning up", "id", r.id)

	result.CloseStdin()
	r.drainProcess(result)

	// Bypass the worker queue; Stop may be draining or closed.
	if currentFile != "" {
		r.uploadDirectly(currentFile)
	}
}

// drainProcess waits for FFmpeg, escalating from wait to SIGTERM to SIGKILL.
// It records stderr on failure.
func (r *GenericRecorder) drainProcess(result *ffmpeg.StartResult) {
	done := make(chan error, 1)
	go func() {
		done <- result.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			r.recordStderr(result)
		}
	case <-time.After(processStopTimeout):
		slog.Warn("recorder ffmpeg did not stop in time, sending signal", "id", r.id)
		_ = result.Signal()

		select {
		case <-done:
		case <-time.After(processKillTimeout):
			slog.Error("recorder ffmpeg force killed", "id", r.id)
			_ = result.Kill()
			<-done
		}
		// After escalation, stderr is both safe to read and most useful.
		r.recordStderr(result)
	}
}

// recordStderr stores FFmpeg's last error line as the recorder's last error.
func (r *GenericRecorder) recordStderr(result *ffmpeg.StartResult) {
	errMsg := util.ExtractLastError(result.Stderr())
	if errMsg == "" {
		return
	}
	r.mu.Lock()
	r.lastError = errMsg
	r.mu.Unlock()
}

// Status returns the recorder process status.
func (r *GenericRecorder) Status() types.ProcessStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return types.ProcessStatus{
		State:      r.state,
		Error:      r.lastError,
		AudioDrops: r.audioDrops.Load(),
	}
}

// PendingUploadCount returns the number of uploads waiting for retry.
func (r *GenericRecorder) PendingUploadCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.retryQueue)
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

	r.config = *cfg
	// Note: S3 client will be recreated on next use if config changed
	// (same pattern as Graph client in notifications)

	return nil
}

// Must be called with r.mu held.
func (r *GenericRecorder) startEncoderLocked() error {
	r.startTime = time.Now()

	// Generate filename based on recording mode
	var filename string
	if r.config.RecordingMode == types.RecordingHourly {
		hourStart := r.startTime.Truncate(time.Hour)
		filename = r.generateFilename(hourStart)
	} else {
		filename = r.generateFilename(r.startTime)
	}

	// Determine output directory based on storage mode
	var outputDir string
	if r.config.StorageMode == types.StorageS3 {
		// S3-only: use durable spool directory
		outputDir = filepath.Join(r.spoolDir, "recorders", r.id)
	} else {
		// Local or Both: use configured LocalPath
		outputDir = r.config.LocalPath
	}
	r.currentFile = filepath.Join(outputDir, filename)

	// Get codec configuration
	codecArgs := types.BuildCodecArgs(r.config.Codec, r.config.Bitrate)
	format := r.config.Codec.Format()
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

	// Scope writer state to this FFmpeg process; rotation creates fresh channels.
	r.audioCh = make(chan []byte, recorderAudioBufferChunks)
	r.writerDone = make(chan struct{})
	go r.audioWriter(result, r.audioCh, r.writerDone)

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)

	// Log new file event (already holding lock)
	p := r.captureLogParamsLocked()
	p.Filename = filepath.Base(r.currentFile)
	r.logEvent(eventlog.RecorderFile, p)
	return nil
}

// stopEncoderAndUpload claims the active FFmpeg process and queues its file.
// If the writer is stuck, it cancels FFmpeg instead of taking stdinMu.
func (r *GenericRecorder) stopEncoderAndUpload() {
	// Claim and clear the process under lock; the winner is the only channel closer.
	r.mu.Lock()
	result := r.result
	audioCh := r.audioCh
	writerDone := r.writerDone
	currentFile := r.currentFile
	r.result = nil
	r.audioCh = nil
	r.writerDone = nil
	r.mu.Unlock()

	if result == nil {
		return
	}

	// Closing audioCh lets the claimed writer drain buffered chunks.
	close(audioCh)

	select {
	case <-writerDone:
		// Writer exited; stdin is idle, so EOF can finalize the file.
		result.CloseStdin()
		r.drainProcess(result)
	case <-time.After(writerDrainTimeout):
		// WriteStdin holds stdinMu; cancel FFmpeg to break the pipe.
		slog.Warn("recorder writer stuck, cancelling ffmpeg to break the pipe", "id", r.id)
		result.Cancel(errRecorderStopped)
		r.drainProcess(result)
		<-writerDone
	}

	// writerDone has fired; remaining buffered chunks are teardown loss.
	if abandoned := len(audioCh); abandoned > 0 {
		drops := r.audioDrops.Add(int64(abandoned))
		slog.Warn("recorder abandoned buffered audio during teardown",
			"id", r.id, "chunks", abandoned, "total_drops", drops)
	}

	// queueForUpload handles local-only and missing S3 config.
	if currentFile != "" {
		r.queueForUpload(currentFile)
	}
}

// Must be called with r.mu held.
func (r *GenericRecorder) scheduleRotationLocked() {
	duration := util.TimeUntilNextHour(time.Now())
	r.rotationTimer = time.AfterFunc(duration, r.rotateFile)
}

// rotateFile handles hourly file rotation, stopping the current encoder and starting a new one.
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

	// Process retry queue at hour boundary
	r.processRetryQueue()

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

// Must be called with r.mu held.
func (r *GenericRecorder) scheduleDurationLimitLocked() {
	duration := time.Duration(r.maxDurationMinutes) * time.Minute
	r.durationTimer = time.AfterFunc(duration, func() {
		slog.Info("recorder max duration reached", "id", r.id, "duration", duration)
		if err := r.Stop(); err != nil {
			slog.Error("failed to stop recorder after max duration", "id", r.id, "error", err)
		}
	})
}

func (r *GenericRecorder) generateFilename(t time.Time) string {
	ext := r.getFileExtension()
	safeName := sanitizeFilename(r.config.Name)
	return fmt.Sprintf("%s-%s.%s", safeName, t.Format("2006-01-02-15-04"), ext)
}

func s3ObjectKey(recorderName, filename string) string {
	// Prefix: recordings/{sanitized-recorder-name}/filename
	safeName := sanitizeFilename(recorderName)
	return fmt.Sprintf("recordings/%s/%s", safeName, filename)
}

func (r *GenericRecorder) getFileExtension() string {
	switch r.config.Codec {
	case types.CodecMP3:
		return "mp3"
	case types.CodecOpus, types.CodecPCM:
		return "ts"
	default:
		slog.Error("unknown recorder codec extension requested, falling back to MPEG-TS extension", "codec", r.config.Codec, "id", r.id)
		return "ts"
	}
}

func (r *GenericRecorder) getContentType() string {
	switch r.config.Codec {
	case types.CodecOpus, types.CodecPCM:
		return "audio/mp2t"
	case types.CodecMP3:
		return "audio/mpeg"
	default:
		slog.Error("unknown recorder codec content type requested, falling back to MPEG-TS content type", "codec", r.config.Codec, "id", r.id)
		return "audio/mp2t"
	}
}

// isS3Configured reports whether S3 is configured for this recorder.
func (r *GenericRecorder) isS3Configured() bool {
	return r.config.S3Bucket != "" && r.config.S3AccessKeyID != "" && r.config.S3SecretAccessKey != ""
}

func (r *GenericRecorder) createS3Client() (*s3.Client, error) {
	return createS3Client(RecorderToS3Config(&r.config))
}

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
