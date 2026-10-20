package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// afterFunc schedules recorder timers; tests replace it to fire callbacks by hand.
var afterFunc = time.AfterFunc

// recordingStat lets tests hold a filename check like an unresponsive network share.
var recordingStat = os.Stat

// checkRecordingPath lets lifecycle tests hold directory validation on a slow mount.
var checkRecordingPath = util.CheckPathWritable

// clockSynced and clockSyncPoll gate the first file on a synchronized clock;
// tests replace them.
var (
	clockSynced   = clockSynchronized
	clockSyncPoll = time.Second
)

// errRecorderStopped marks intentional stop/rotation cancellation.
var errRecorderStopped = errors.New("recorder stopped")

const (
	// recorderAudioBufferChunks bounds backlog to about 3s of audio.
	recorderAudioBufferChunks = 30
	// usedFilenameWindow is how long a started filename stays reserved. A
	// minute-based name only recurs within its minute or in the repeated hour
	// of a DST fall-back.
	usedFilenameWindow = 2 * time.Hour
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

	// prepareLocks serializes output preparation per directory; preparation
	// runs without mu. Protected by mu; one entry per directory ever used.
	prepareLocks map[string]*sync.Mutex

	id                 string
	config             types.Recorder
	ffmpegPath         string
	maxDurationMinutes int // For on-demand mode (from global config)
	eventLogger        *eventlog.Logger
	onUploadAbandoned  UploadAbandonedCallback

	spoolDir  string
	state     types.ProcessState
	lastError string
	startGen  uint64 // Identifies the latest Start; an older start must not launch.

	// FFmpeg process (encapsulates cmd, stdin, stderr with thread-safe access)
	result *ffmpeg.StartResult

	// Per-process writer state; reset with each FFmpeg process.
	audioMu    sync.RWMutex // Protects audioCh from capture/close; never held across I/O.
	audioCh    chan []byte
	writerDone chan struct{}
	recording  atomic.Bool  // Capture must not take mu behind filesystem/process startup.
	audioDrops atomic.Int64 // Lost chunks from overflow or teardown.

	// Current recording
	currentFile   string
	usedFilenames map[string]time.Time // Names started within usedFilenameWindow by this process; S3-only mode deletes the file itself after upload.

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
	retryProcessMu sync.Mutex // Only one retry batch may own paths at a time.
	retryQueue     []pendingUpload
	retryInFlight  map[string]struct{} // Paths remain owned while temporarily absent from retryQueue; protected by mu.
	retryRequested bool                // A pass asked for during a batch runs after it; protected by mu.
	retryEnqueueMu sync.Mutex          // Serializes persistence/publication without holding the state lock over disk I/O.

	// Rotation timer (hourly mode)
	rotationTimer      *time.Timer
	rotationGeneration uint64
	// retiring tracks files being finalized; Stop waits until their uploads
	// are queued or durably registered for recovery before intake closes.
	retiring      sync.WaitGroup
	retiringFiles map[string]struct{} // Files rotations are finalizing; protected from cleanup until queued.

	// Max duration timer (on-demand mode)
	durationTimer      *time.Timer
	durationGeneration uint64
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
func NewGenericRecorder(cfg GenericRecorderConfig) *GenericRecorder {
	return &GenericRecorder{
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
		usedFilenames:      make(map[string]time.Time),
		retiringFiles:      make(map[string]struct{}),
	}
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

// getOrCreateS3Client returns the cached S3 client, recreating it when the
// config changed. It returns nil when S3 is not configured.
func (r *GenericRecorder) getOrCreateS3Client() *s3.Client {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isS3Configured() {
		return nil
	}

	newKey := s3ConfigKeyFrom(&r.config)

	// Reuse cached client if config unchanged
	if r.s3Client == nil || r.s3ConfigKey != newKey {
		r.s3Client = r.createS3Client()
		r.s3ConfigKey = newKey
	}
	return r.s3Client
}

// IsCurrentFile reports whether the recorder is still writing or finalizing
// path, so retention cleanup must leave it alone.
func (r *GenericRecorder) IsCurrentFile(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, retiring := r.retiringFiles[path]
	return path != "" && (path == r.currentFile || retiring)
}

// Start starts the recorder.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == types.ProcessRunning || r.state == types.ProcessStarting {
		return ErrAlreadyRecording
	}
	if r.state == types.ProcessStopping {
		return ErrRecorderStopping
	}

	// Clear any previous error and set starting state. A stale start can still
	// move a run to Error without cancelling its limit, so cancel it here too.
	r.cancelDurationLimitLocked()
	r.lastError = ""
	r.audioDrops.Store(0)
	r.state = types.ProcessStarting

	// Start async validation and startup
	r.startGen++
	go r.startAsync(r.startGen)

	return nil
}

// waitForClockSync holds the first file until the system clock is synchronized:
// file names and retention dates come from it, and a Raspberry Pi has no
// real-time clock. It reports false when Stop or a newer Start took over.
func (r *GenericRecorder) waitForClockSync(gen uint64) bool {
	if clockSynced() {
		return true
	}
	slog.Warn("system clock is not synchronized, recording starts once it is", "id", r.id)
	for !clockSynced() {
		r.mu.Lock()
		current := r.state == types.ProcessStarting && r.startGen == gen
		if current {
			r.lastError = "waiting for system clock synchronization"
		}
		r.mu.Unlock()
		if !current {
			return false
		}
		time.Sleep(clockSyncPoll)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.state == types.ProcessStarting && r.startGen == gen
	if current {
		r.lastError = ""
	}
	return current
}

func (r *GenericRecorder) startAsync(gen uint64) {
	// Wait before validating, so a configuration changed meanwhile is the one checked.
	if !r.waitForClockSync(gen) {
		return
	}

	// Now lock and start the encoder
	r.mu.Lock()

	// Re-check state in case Stop() or a newer Start() took over during validation
	if r.state != types.ProcessStarting || r.startGen != gen {
		r.mu.Unlock()
		return
	}

	// Start FFmpeg encoder
	path, current, err := r.prepareFilenameLocked(time.Now())
	if !current {
		r.mu.Unlock()
		return
	}
	if err == nil {
		err = r.startEncoderLocked(path)
	}
	if err != nil {
		r.state = types.ProcessError
		r.recording.Store(false)
		r.lastError = err.Error()
		params := r.captureLogParamsLocked()
		params.Error = err.Error()
		r.mu.Unlock()
		slog.Error("recorder failed to start encoder", "id", r.id, "error", err)
		r.logEvent(eventlog.RecorderError, params)
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
		r.scheduleRotationLocked(time.Now())
	} else if r.maxDurationMinutes > 0 {
		r.scheduleDurationLimitLocked()
	}

	r.state = types.ProcessRunning
	r.recording.Store(true)

	// Capture log params while holding lock
	logParams := r.captureLogParamsLocked()
	name := r.config.Name
	mode := r.config.RecordingMode
	r.mu.Unlock()

	slog.Info("recorder started", "id", r.id, "name", name, "mode", mode)
	r.logEvent(eventlog.RecorderStarted, logParams)
}

func (r *GenericRecorder) reportError(file string, err error) {
	r.mu.Lock()
	p := r.recordErrorLocked(file, err)
	r.mu.Unlock()
	r.emitError(p, err)
}

// recordErrorLocked sets the status error and returns its event details, so a
// caller that has just checked ownership under r.mu cannot be overtaken by a
// newer run before the status changes. Must be called with r.mu held.
func (r *GenericRecorder) recordErrorLocked(file string, err error) *eventlog.RecorderDetails {
	r.lastError = err.Error()
	p := r.captureLogParamsLocked()
	p.Error = err.Error()
	if file != "" {
		p.Filename = filepath.Base(file)
	}
	return p
}

// emitError logs an error recorded by recordErrorLocked; call it without r.mu.
func (r *GenericRecorder) emitError(p *eventlog.RecorderDetails, err error) {
	slog.Error("recorder error", "id", r.id, "file", p.Filename, "error", err)
	r.logEvent(eventlog.RecorderError, p)
}

// Must be called with r.mu held.
func (r *GenericRecorder) captureLogParamsLocked() *eventlog.RecorderDetails {
	return &eventlog.RecorderDetails{
		RecorderName: r.config.Name,
		Codec:        string(r.config.Codec),
		StorageMode:  string(r.config.StorageMode),
	}
}

func (r *GenericRecorder) logEvent(eventType eventlog.EventType, p *eventlog.RecorderDetails) {
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
	return r.stopLocked()
}

// stopLocked must be called with r.mu held; it unlocks r.mu before waiting
// for cleanup.
func (r *GenericRecorder) stopLocked() error {
	// Already stopped or stopping
	if r.state == types.ProcessStopped || r.state == types.ProcessStopping {
		r.mu.Unlock()
		return nil
	}

	r.state = types.ProcessStopping
	r.recording.Store(false)

	// Always stop timers (may be running even after write error)
	if r.rotationTimer != nil {
		r.rotationTimer.Stop()
		r.rotationTimer = nil
	}
	r.rotationGeneration++
	r.cancelDurationLimitLocked()
	r.mu.Unlock()

	// Queue the final file, and any file a rotation is still finalizing,
	// before stopping the worker. Stopping state keeps new rotations out.
	r.stopEncoderAndUpload()
	r.retiring.Wait()

	// Nothing should queue uploads after this point; uploadClosed still sends
	// a late file to retry storage.
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
	// Keep uploadQueue stable; uploadClosed reroutes late files.
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
// The caller must not mutate pcm after the call. Full buffers drop the newest
// chunk and increment AudioDrops.
func (r *GenericRecorder) WriteAudio(pcm []byte) {
	// Only the short audio lock serializes capture with channel hand-over/close.
	r.audioMu.RLock()

	ch := r.audioCh
	if ch == nil || !r.recording.Load() {
		r.audioMu.RUnlock()
		return
	}

	select {
	case ch <- pcm:
		r.audioMu.RUnlock()
		return
	default:
	}
	// Count under the lock so a drop never lands in the next recording's counter.
	drops := r.audioDrops.Add(1)
	r.audioMu.RUnlock()
	if drops == 1 || drops%100 == 0 {
		slog.Warn("recorder audio buffer full, dropping chunk; recording will have a gap",
			"id", r.id, "total_drops", drops)
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

		// Stop and rotation intentionally close this path.
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
	// Stop or rotation owns cleanup for swapped processes.
	if r.result != result {
		r.mu.Unlock()
		return
	}
	// A running Stop owns the state; Error would let Start overlap its teardown.
	if r.state != types.ProcessStopping {
		r.state = types.ProcessError
	}
	// The run is over; its duration limit must not later stop or clear anything.
	r.cancelDurationLimitLocked()
	r.recording.Store(false)
	r.lastError = writeErr.Error()
	capturedFile := r.currentFile
	r.result = nil
	r.currentFile = ""
	r.audioMu.Lock()
	r.audioCh = nil
	r.audioMu.Unlock()
	// audioWriter closes writerDone on return.
	r.writerDone = nil
	r.retiring.Add(1)
	r.retiringFiles[capturedFile] = struct{}{}
	r.mu.Unlock()

	// Let audioWriter return before cleanup waits on FFmpeg.
	go r.cleanupAfterWriteError(result, capturedFile, writeErr)
}

// cleanupAfterWriteError closes a broken FFmpeg process and preserves its partial file for upload.
// The failing write has released stdinMu, so CloseStdin cannot deadlock.
func (r *GenericRecorder) cleanupAfterWriteError(result *ffmpeg.StartResult, currentFile string, writeErr error) {
	defer r.retiring.Done()
	defer func() {
		r.mu.Lock()
		delete(r.retiringFiles, currentFile)
		r.mu.Unlock()
	}()
	result.CloseStdin()
	if err := r.drainProcess(result); err != nil {
		writeErr = err
	}
	r.reportError(currentFile, writeErr)

	// Persist recovery before releasing Stop; network retries run independently.
	if currentFile != "" && result.BytesWritten() > 0 && !outputRefused(result.Stderr()) {
		// Accepted stdin does not mean FFmpeg wrote anything to the file.
		if req, ok := r.prepareUploadRequest(currentFile); ok && req.fileSize > 0 {
			r.addToRetryQueue(req, writeErr.Error())
			go r.processRetryQueue()
		}
	}
}

// drainProcess waits for FFmpeg, escalating through SIGTERM and SIGKILL.
func (r *GenericRecorder) drainProcess(result *ffmpeg.StartResult) error {
	err := result.WaitEscalating(processStopTimeout, processKillTimeout,
		func() {
			slog.Warn("recorder ffmpeg did not stop in time, sending signal", "id", r.id)
		},
		func() { slog.Error("recorder ffmpeg force killed", "id", r.id) })

	// FFmpeg's -n refusal can exit successfully, even after accepting stdin.
	if line, ok := refusalLine(result.Stderr()); ok {
		return errors.New(line)
	}
	if err != nil {
		if message := util.ExtractLastError(result.Stderr()); message != "" {
			return fmt.Errorf("%s: %w", message, err)
		}
	}
	return err
}

func outputRefused(stderr string) bool {
	_, ok := refusalLine(stderr)
	return ok
}

// refusalLine finds FFmpeg's -n refusal among warnings and the error lines that follow it.
func refusalLine(stderr string) (string, bool) {
	for line := range strings.Lines(stderr) {
		if strings.Contains(line, "already exists. Exiting.") {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
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
	return r.recording.Load()
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

	// Leaving hourly mode cancels the pending rotation, including one already
	// preparing its next file.
	if r.config.RecordingMode == types.RecordingHourly && cfg.RecordingMode != types.RecordingHourly {
		if r.rotationTimer != nil {
			r.rotationTimer.Stop()
			r.rotationTimer = nil
		}
		r.rotationGeneration++
		if r.state == types.ProcessRunning && r.maxDurationMinutes > 0 {
			r.scheduleDurationLimitLocked()
		}
	} else if cfg.RecordingMode == types.RecordingHourly && r.config.RecordingMode != types.RecordingHourly &&
		r.state == types.ProcessRunning {
		r.cancelDurationLimitLocked()
		r.scheduleRotationLocked(time.Now())
	}
	r.config = *cfg
	// Note: S3 client will be recreated on next use if config changed
	// (same pattern as Graph client in notifications)

	return nil
}

// prepareFilenameLocked releases mu for filesystem checks, then revalidates
// ownership and configuration. Stop need not join a blocked check, and a stale
// result cannot start an encoder. Called and returned with mu held.
func (r *GenericRecorder) prepareFilenameLocked(fileTime time.Time) (path string, current bool, err error) {
	gen, state := r.startGen, r.state
	for {
		cfg := r.config
		outputDir := cfg.LocalPath
		if cfg.StorageMode == types.StorageS3 {
			outputDir = filepath.Join(r.spoolDir, "recorders", r.id)
		}
		base := r.generateFilename(fileTime)
		used := maps.Clone(r.usedFilenames)
		lock := r.prepareLockLocked(outputDir)
		r.mu.Unlock()
		// A check hung on a directory must not gain another blocked syscall
		// with every Start after a Stop; another directory is not held up.
		lock.Lock()
		filename := base
		err := prepareRecordingDir(cfg.StorageMode, outputDir)
		if err == nil {
			filename = nextRecordingFilename(base, func(name string) bool {
				if _, exists := used[name]; exists {
					return true
				}
				_, err := recordingStat(filepath.Join(outputDir, name))
				return err == nil
			})
		}
		lock.Unlock()
		r.mu.Lock()
		if r.startGen != gen || r.state != state {
			return "", false, nil
		}
		if r.config != cfg {
			continue // A concurrent edit must not launch into the old path/name.
		}
		return filepath.Join(outputDir, filename), true, err
	}
}

// prepareLockLocked returns the preparation lock of dir. Must be called with
// r.mu held.
func (r *GenericRecorder) prepareLockLocked(dir string) *sync.Mutex {
	if r.prepareLocks == nil {
		r.prepareLocks = make(map[string]*sync.Mutex)
	}
	lock := r.prepareLocks[dir]
	if lock == nil {
		lock = new(sync.Mutex)
		r.prepareLocks[dir] = lock
	}
	return lock
}

func prepareRecordingDir(storageMode types.StorageMode, dir string) error {
	if err := util.ValidatePath("recording directory", dir); err != nil {
		return err
	}
	if storageMode == types.StorageS3 {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Spool directory needs to be readable.
			return fmt.Errorf("failed to create spool directory: %w", err)
		}
		return nil
	}
	// CheckPathWritable logs the cause; its error adds nothing for the status.
	if checkRecordingPath(dir) != nil {
		return errors.New("local path is not writable")
	}
	return nil
}

// startEncoderLocked starts FFmpeg on the prepared output path.
// Must be called with r.mu held, after revalidating file preparation.
func (r *GenericRecorder) startEncoderLocked(path string) error {
	r.currentFile = path

	// Get codec configuration
	codecArgs := types.BuildCodecArgs(r.config.Codec, r.config.Bitrate)
	format := r.config.Codec.Format()

	// Build FFmpeg command args
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-c:a")
	args = append(args, codecArgs...)
	args = append(args,
		"-f", format,
		"-hide_banner",
		"-loglevel", "warning",
		"-n",
		r.currentFile,
	)

	// Start FFmpeg process
	result, err := ffmpeg.StartProcess(r.ffmpegPath, args)
	if err != nil {
		return err
	}

	r.rememberFilenameLocked(filepath.Base(path))
	r.result = result

	// Scope writer state to this FFmpeg process; rotation creates fresh channels.
	r.audioMu.Lock()
	r.audioCh = make(chan []byte, recorderAudioBufferChunks)
	r.audioMu.Unlock()
	r.writerDone = make(chan struct{})
	go r.audioWriter(result, r.audioCh, r.writerDone)

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)

	// Log new file event (already holding lock)
	p := r.captureLogParamsLocked()
	p.Filename = filepath.Base(r.currentFile)
	r.logEvent(eventlog.RecorderFile, p)
	return nil
}

// encoderRun is one FFmpeg process with its writer state and output file.
type encoderRun struct {
	result     *ffmpeg.StartResult
	audioCh    chan []byte
	writerDone chan struct{}
	file       string
}

// claimEncoderLocked detaches the active FFmpeg process so that the caller
// alone finishes it. Must be called with r.mu held.
func (r *GenericRecorder) claimEncoderLocked() encoderRun {
	run := encoderRun{
		result:     r.result,
		audioCh:    r.audioCh,
		writerDone: r.writerDone,
		file:       r.currentFile,
	}
	r.result = nil
	// audioCh stays: a rotation keeps feeding it until the replacement is ready.
	r.writerDone = nil
	return run
}

// stopEncoderAndUpload claims the active FFmpeg process and queues its file.
func (r *GenericRecorder) stopEncoderAndUpload() {
	r.mu.Lock()
	run := r.claimEncoderLocked()
	r.audioMu.Lock()
	r.audioCh = nil
	r.audioMu.Unlock()
	r.mu.Unlock()
	r.finishEncoder(run)
}

// finishEncoder closes a claimed FFmpeg process and queues its file.
// Stuck writers are cancelled instead of taking stdinMu.
func (r *GenericRecorder) finishEncoder(run encoderRun) {
	if run.result == nil {
		return
	}

	// The claimer is the only channel closer; closing lets the writer drain
	// buffered chunks.
	r.audioMu.Lock()
	close(run.audioCh)
	r.audioMu.Unlock()

	var finalizeErr error
	select {
	case <-run.writerDone:
		// Writer exited; stdin is idle, so EOF can finalize the file.
		run.result.CloseStdin()
		finalizeErr = r.drainProcess(run.result)
	case <-time.After(writerDrainTimeout):
		// WriteStdin holds stdinMu; cancel FFmpeg to unblock it.
		slog.Warn("recorder writer stuck, cancelling ffmpeg to break the pipe", "id", r.id, "file", filepath.Base(run.file))
		run.result.Cancel(errRecorderStopped)
		finalizeErr = r.drainProcess(run.result)
		<-run.writerDone
	}
	if finalizeErr != nil && !errors.Is(context.Cause(run.result.Context()), errRecorderStopped) {
		r.reportError(run.file, finalizeErr)
	}

	// writerDone has fired; remaining buffered chunks are teardown loss.
	if abandoned := len(run.audioCh); abandoned > 0 {
		drops := r.audioDrops.Add(int64(abandoned))
		slog.Warn("recorder abandoned buffered audio during teardown",
			"id", r.id, "file", filepath.Base(run.file), "chunks", abandoned, "total_drops", drops)
	}

	if run.file == "" || outputRefused(run.result.Stderr()) {
		return
	}
	if run.result.BytesWritten() == 0 {
		// Only successful finalization proves this is our empty output to remove.
		if finalizeErr != nil {
			return
		}
		if err := os.Remove(run.file); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.reportError(run.file, fmt.Errorf("remove empty recording: %w", err))
		} else {
			slog.Info("discarded recording without audio", "id", r.id, "file", filepath.Base(run.file))
		}
		return
	}
	// queueForUpload handles local-only and missing S3 config.
	r.queueForUpload(run.file)
}

// scheduleRotationLocked arms the rotation for the first hour boundary after
// from. Must be called with r.mu held.
func (r *GenericRecorder) scheduleRotationLocked(from time.Time) {
	if r.rotationTimer != nil {
		r.rotationTimer.Stop()
	}
	r.rotationGeneration++
	generation := r.rotationGeneration
	next := from.Truncate(time.Hour).Add(time.Hour)
	r.rotationTimer = afterFunc(time.Until(next), func() { r.rotateFile(generation) })
}

// rotateFile hands the recording over to a new file at the hour boundary.
// The next FFmpeg process starts and takes over the audio before the current
// one is closed, so no audio is lost between the two files.
func (r *GenericRecorder) rotateFile(generation uint64) {
	r.mu.Lock()
	if r.state != types.ProcessRunning || generation != r.rotationGeneration {
		r.mu.Unlock()
		return
	}

	// The wall clock names the file: a timer that fires a moment early still
	// gives HH-00, a late one its real start, and after a clock step the name
	// and the next boundary follow the corrected time.
	at := time.Now().Round(time.Minute)
	r.rotationGeneration++ // Claim this callback before releasing mu for preparation.
	claimedGeneration := r.rotationGeneration
	path, current, prepareErr := r.prepareFilenameLocked(at)
	if !current || r.rotationGeneration != claimedGeneration {
		r.mu.Unlock()
		return
	}
	if prepareErr != nil {
		r.scheduleRotationLocked(at)
		p := r.recordErrorLocked(path, prepareErr)
		r.mu.Unlock()
		r.emitError(p, prepareErr)
		return
	}

	// Capture keeps feeding the current channel during preparation; only
	// the channel hand-over takes the separate audio lock.
	previous := r.claimEncoderLocked()
	r.retiring.Add(1)
	defer r.retiring.Done()
	startErr := r.startEncoderLocked(path)
	var failure *eventlog.RecorderDetails
	if startErr != nil {
		failure = r.recordErrorLocked(r.currentFile, startErr)
		// The current encoder still runs: keep recording into its file instead
		// of stopping until the hourly retry.
		r.result, r.writerDone, r.currentFile = previous.result, previous.writerDone, previous.file
		previous = encoderRun{}
	} else {
		r.retiringFiles[previous.file] = struct{}{}
		r.lastError = "" // Clears the error of an earlier failed rotation.
		slog.Info("recorder rotated file", "id", r.id, "from", filepath.Base(previous.file), "to", filepath.Base(r.currentFile))
	}
	r.scheduleRotationLocked(at)
	r.mu.Unlock()
	if failure != nil {
		r.emitError(failure, startErr)
	}

	r.finishEncoder(previous)
	r.mu.Lock()
	delete(r.retiringFiles, previous.file)
	r.mu.Unlock()

	// Process the retry queue off the rotation path: a slow or unreachable S3
	// endpoint may block for many minutes per pending file, and Stop waits for
	// this rotation through retiring. Safe concurrently: an overlapping run
	// returns at once while a batch owns its paths.
	go r.processRetryQueue()
}

// Must be called with r.mu held.
func (r *GenericRecorder) scheduleDurationLimitLocked() {
	r.cancelDurationLimitLocked()
	generation := r.durationGeneration
	duration := time.Duration(r.maxDurationMinutes) * time.Minute
	r.durationTimer = afterFunc(duration, func() {
		r.mu.Lock()
		if generation != r.durationGeneration {
			r.mu.Unlock()
			return
		}
		slog.Info("recorder max duration reached", "id", r.id, "duration", duration)
		if err := r.stopLocked(); err != nil {
			slog.Error("failed to stop recorder after max duration", "id", r.id, "error", err)
		}
	})
}

// Must be called with r.mu held.
func (r *GenericRecorder) cancelDurationLimitLocked() {
	if r.durationTimer != nil {
		r.durationTimer.Stop()
		r.durationTimer = nil
	}
	r.durationGeneration++
}

func (r *GenericRecorder) generateFilename(t time.Time) string {
	safeName := util.SanitizeFilename(r.config.Name)
	return fmt.Sprintf("%s-%s.%s", safeName, t.Format(recordingTimestampLayout), r.config.Codec.FileExtension())
}

// nextRecordingFilename returns name, or the first of name-2, name-3, ...
// that is not taken.
func nextRecordingFilename(name string, taken func(string) bool) string {
	candidate := name
	for suffix := 2; taken(candidate); suffix++ {
		candidate = withNumericSuffix(name, suffix)
	}
	return candidate
}

// withNumericSuffix inserts "-<n>" before the extension of a filename or S3 key.
func withNumericSuffix(name string, n int) string {
	extension := filepath.Ext(name)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, extension), n, extension)
}

// rememberFilenameLocked reserves filename and forgets names older than
// usedFilenameWindow. Must be called with r.mu held.
func (r *GenericRecorder) rememberFilenameLocked(filename string) {
	now := time.Now()
	maps.DeleteFunc(r.usedFilenames, func(_ string, started time.Time) bool {
		return now.Sub(started) > usedFilenameWindow
	})
	r.usedFilenames[filename] = now
}

func s3ObjectKey(recorderName, filename string) string {
	// Prefix: recordings/{sanitized-recorder-name}/filename
	safeName := util.SanitizeFilename(recorderName)
	return fmt.Sprintf("recordings/%s/%s", safeName, filename)
}

// isS3Configured reports whether S3 is configured for this recorder.
func (r *GenericRecorder) isS3Configured() bool {
	return r.config.HasS3Credentials()
}

func (r *GenericRecorder) createS3Client() *s3.Client {
	return createS3Client(RecorderToS3Config(&r.config))
}

// TestRecorderS3Connection tests S3 connectivity for a recorder configuration.
func TestRecorderS3Connection(cfg *types.Recorder) error {
	return TestS3Connection(RecorderToS3Config(cfg))
}
