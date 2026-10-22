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

// reserveRecordingFile exclusively creates path; tests replace it to hold a
// reservation like an unresponsive network share.
var reserveRecordingFile = func(path string) (*os.File, error) {
	if err := util.ValidatePath("recording file", path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666) //nolint:gosec // Preserve FFmpeg's output permissions (restricted by umask/ACLs); path uses a validated directory and sanitized filename.
}

// checkRecordingPath lets lifecycle tests hold directory validation on a slow mount.
var checkRecordingPath = util.CheckPathWritable

// removeEmptyRecording lets tests reproduce a failed unlink without relying on mount permissions.
var removeEmptyRecording = os.Remove

// syncInterval is how often a running recording is synced to disk (D4).
const syncInterval = 30 * time.Second

// awaitStop waits for a Stop already in progress; tests replace it to see a
// caller join that teardown.
var awaitStop = func(done <-chan struct{}) { <-done }

// writerDrainTimeout bounds graceful writer drain; tests shorten it.
var writerDrainTimeout = 5 * time.Second

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
	// processStopTimeout bounds waiting before escalating to SIGTERM.
	processStopTimeout = 10 * time.Second
	// processKillTimeout bounds waiting after SIGTERM before SIGKILL.
	processKillTimeout = 2 * time.Second
)

// GenericRecorder saves audio to files with optional S3 upload.
type GenericRecorder struct {
	mu sync.RWMutex // Protects state, config, file paths

	// reconcileMu serializes Manager.UpdateRecorder's runtime reconciliation
	// and Manager.StopRecorder for this recorder, including a blocking Stop,
	// so updates change the config and stop or start the recorder in the same
	// order. Encoder.UpdateRecorder persists the config before this lock, so two
	// simultaneous saves may be stored and applied in different orders.
	// Lock order: reconcileMu, then Manager.mu, then mu.
	reconcileMu sync.Mutex

	// prepareLocks serializes output preparation per directory; preparation
	// runs without mu. Protected by mu; one entry per directory ever used.
	prepareLocks map[string]*sync.Mutex

	id                 string
	config             types.Recorder
	ffmpegPath         string
	maxDurationMinutes int // For on-demand mode (from global config)
	eventLogger        *eventlog.Logger
	outbox             *outbox

	spoolDir  string
	state     types.ProcessState
	lastError string
	startGen  uint64        // Identifies the latest Start; an older start must not launch.
	stopDone  chan struct{} // Closed when the Stop that set ProcessStopping finishes.

	result *ffmpeg.StartResult

	// Per-process writer state; reset with each FFmpeg process.
	audioMu    sync.RWMutex // Protects audioCh from capture/close; never held across I/O.
	audioCh    chan []byte
	writerDone chan struct{}
	recording  atomic.Bool  // Capture must not take mu behind filesystem/process startup.
	audioDrops atomic.Int64 // Lost chunks from overflow or teardown.

	currentFile   string
	currentOutput *recordingOutput     // Exclusively created file owned by the current encoder.
	usedFilenames map[string]time.Time // Names started within usedFilenameWindow by this process; S3-only mode deletes the file itself after upload.

	stopSync func() // Stops and joins the current encoder's periodic sync.

	s3Client    *s3.Client // Retention's client; uploads use the outbox destinations.
	s3ConfigKey string     // Config key used to create cached client

	// Rotation timer (hourly mode)
	rotationTimer      *time.Timer
	rotationGeneration uint64
	// retiring tracks files being finalized; Stop waits until their tickets
	// are ready or failed.
	retiring sync.WaitGroup

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
	Outbox             *outbox
}

// NewGenericRecorder creates a new recorder instance.
func NewGenericRecorder(cfg GenericRecorderConfig) *GenericRecorder {
	recorder := *cfg.Recorder
	recorder.InitializeFileIdentity()
	return &GenericRecorder{
		id:                 cfg.Recorder.ID,
		config:             recorder,
		ffmpegPath:         cfg.FFmpegPath,
		maxDurationMinutes: cfg.MaxDurationMinutes,
		eventLogger:        cfg.EventLogger,
		outbox:             cfg.Outbox,
		spoolDir:           cfg.SpoolDir,
		state:              types.ProcessStopped,
		usedFilenames:      make(map[string]time.Time),
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

// IsCurrentFile reports whether path is the recorder's current file.
func (r *GenericRecorder) IsCurrentFile(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return path != "" && path == r.currentFile
}

// Start launches asynchronous clock validation and encoder startup.
func (r *GenericRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked()
}

// startHourly starts the recorder if its config is still enabled and hourly,
// and does nothing otherwise. The config is checked under the lock
// UpdateConfig takes, so a concurrent update cannot pass in between.
func (r *GenericRecorder) startHourly() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.config.Enabled || r.config.RecordingMode != types.RecordingHourly {
		return nil
	}
	return r.startLocked()
}

// startOnDemand starts the recorder if its config is still enabled and
// on-demand, checked under mu like startHourly; otherwise it returns
// ErrHourlyRecorderNotControllable or ErrRecorderDisabled.
func (r *GenericRecorder) startOnDemand() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.RecordingMode == types.RecordingHourly {
		return ErrHourlyRecorderNotControllable
	}
	if !r.config.Enabled {
		return ErrRecorderDisabled
	}
	return r.startLocked()
}

// startLocked must be called with r.mu held.
func (r *GenericRecorder) startLocked() error {
	if r.state == types.ProcessRunning || r.state == types.ProcessStarting {
		return ErrAlreadyRecording
	}
	if r.state == types.ProcessStopping {
		return ErrRecorderStopping
	}

	// An earlier failed run may still have a duration timer armed.
	r.cancelDurationLimitLocked()
	r.lastError = ""
	r.audioDrops.Store(0)
	r.state = types.ProcessStarting

	r.startGen++
	go r.startAsync(r.startGen)

	return nil
}

// waitForClockSync delays startup to keep boot-time clock errors out of filenames
// and retention dates. Callers must recheck run ownership after it returns.
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

	r.mu.Lock()

	// Clock synchronization may have yielded to Stop or a newer Start.
	if r.state != types.ProcessStarting || r.startGen != gen {
		r.mu.Unlock()
		return
	}

	output, current, err := r.prepareFilenameLocked(time.Now())
	if !current {
		r.mu.Unlock()
		return
	}
	if err == nil {
		err = r.startEncoderLocked(output)
	}
	if err != nil {
		r.state = types.ProcessError
		r.recording.Store(false)
		r.lastError = err.Error()
		params := r.captureLogParamsLocked()
		params.Error = err.Error()
		r.mu.Unlock()
		output.discard()
		slog.Error("recorder failed to start encoder", "id", r.id, "error", err)
		r.logEvent(eventlog.RecorderError, params)
		return
	}

	if r.config.RecordingMode == types.RecordingHourly {
		r.scheduleRotationLocked(time.Now())
	} else if r.maxDurationMinutes > 0 {
		r.scheduleDurationLimitLocked()
	}

	r.state = types.ProcessRunning
	r.recording.Store(true)

	// Snapshot event fields before releasing mu.
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

// recordErrorLocked updates the status error before a newer run can take over.
// The caller must hold r.mu; emit the returned event after unlocking.
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

// Stop finalizes recordings; their uploads are the uploader's, so Stop never
// waits for S3.
// Calls made while already stopping wait for that Stop to finish (finalizing,
// not S3).
func (r *GenericRecorder) Stop() error {
	r.mu.Lock()
	return r.stopLocked()
}

// stopLocked must be called with r.mu held; it unlocks r.mu before waiting
// for cleanup.
func (r *GenericRecorder) stopLocked() error {
	if r.state == types.ProcessStopped {
		r.mu.Unlock()
		return nil
	}
	// Join the teardown in progress, so Manager.Stop cannot return before a
	// Stop started elsewhere (such as a disabling update) has finalized.
	if r.state == types.ProcessStopping {
		done := r.stopDone
		r.mu.Unlock()
		awaitStop(done)
		return nil
	}

	r.state = types.ProcessStopping
	r.stopDone = make(chan struct{})
	r.recording.Store(false)

	// Always stop timers (may be running even after write error)
	if r.rotationTimer != nil {
		r.rotationTimer.Stop()
		r.rotationTimer = nil
	}
	r.rotationGeneration++
	r.cancelDurationLimitLocked()
	r.mu.Unlock()

	// Finalize the final file, and any file a rotation is still finalizing.
	// Stopping state keeps new rotations out. Uploads are the uploader's.
	r.stopEncoderAndUpload()
	r.retiring.Wait()

	r.mu.Lock()
	r.state = types.ProcessStopped
	close(r.stopDone)
	r.lastError = ""

	// Snapshot event fields before releasing mu.
	logParams := r.captureLogParamsLocked()
	name := r.config.Name
	r.mu.Unlock()

	slog.Info("recorder stopped", "id", r.id, "name", name)
	r.logEvent(eventlog.RecorderStopped, logParams)
	return nil
}

// stopIn calls Stop only in state: for ProcessError it tears down the failed
// run, for ProcessStopping it waits for the Stop in progress. Any other state
// is left alone.
func (r *GenericRecorder) stopIn(state types.ProcessState) error {
	r.mu.Lock()
	if r.state != state {
		r.mu.Unlock()
		return nil
	}
	return r.stopLocked()
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
	run := r.claimEncoderLocked() // audioWriter closes writerDone on return.
	r.currentFile = ""
	r.audioMu.Lock()
	r.audioCh = nil
	r.audioMu.Unlock()
	r.retiring.Add(1)
	r.mu.Unlock()

	// Let audioWriter return before cleanup waits on FFmpeg.
	go r.cleanupAfterWriteError(run, writeErr)
}

// cleanupAfterWriteError preserves failed outputs in quarantine for inspection.
// The failing write has released stdinMu, so CloseStdin cannot deadlock.
func (r *GenericRecorder) cleanupAfterWriteError(run encoderRun, writeErr error) {
	defer r.retiring.Done()
	run.stopSync()
	run.result.CloseStdin()
	if err := r.drainProcess(run.result); err != nil {
		writeErr = err
	}
	r.failOutput(run, writeErr)
}

// failOutput releases a failed output and reports it once. The output keeps its
// marker for inspection; an untouched placeholder holds nothing to inspect.
func (r *GenericRecorder) failOutput(run encoderRun, cause error) {
	size, ownErr := run.output.release()
	err := errors.Join(cause, ownErr)
	if ownErr == nil && size == 0 {
		if discardErr := r.discardEmpty(run); discardErr != nil {
			err = errors.Join(err, discardErr)
			run.output.recordFailure(err)
		}
	} else {
		run.output.recordFailure(err)
	}
	r.reportError(run.file, err)
}

// drainProcess waits for FFmpeg, escalating through SIGTERM and SIGKILL.
func (r *GenericRecorder) drainProcess(result *ffmpeg.StartResult) error {
	err := result.WaitEscalating(processStopTimeout, processKillTimeout,
		func() {
			slog.Warn("recorder ffmpeg did not stop in time, sending signal", "id", r.id)
		},
		func() { slog.Error("recorder ffmpeg force killed", "id", r.id) })

	if err != nil {
		if message := util.ExtractLastError(result.Stderr()); message != "" {
			return fmt.Errorf("%s: %w", message, err)
		}
	}
	return err
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

// IsRecording reports whether recording is currently in progress.
func (r *GenericRecorder) IsRecording() bool {
	return r.recording.Load()
}

// UpdateConfig updates the recorder configuration. It leaves the run state,
// including ProcessError, to Manager.UpdateRecorder.
func (r *GenericRecorder) UpdateConfig(cfg *types.Recorder) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.ID != r.id {
		return fmt.Errorf("recorder ID cannot change")
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
	updated := *cfg
	updated.IDBasedFiles, updated.LegacyFilePrefix = true, r.config.LegacyFilePrefix
	r.config = updated
	// getOrCreateS3Client refreshes changed connection settings on next use.

	return nil
}

// prepareFilenameLocked reserves an output with mu released so Stop stays responsive.
// It rechecks run ownership and config before returning; false means the run ended.
// Called and returned with mu held.
func (r *GenericRecorder) prepareFilenameLocked(fileTime time.Time) (*recordingOutput, bool, error) {
	gen, state := r.startGen, r.state
	for r.startGen == gen && r.state == state {
		cfg := r.config
		outputDir := cfg.LocalPath
		if cfg.StorageMode == types.StorageS3 {
			outputDir = filepath.Join(r.spoolDir, "recordings", cfg.RecordingFilePrefix())
		}
		base := r.generateFilename(fileTime)
		used := maps.Clone(r.usedFilenames)
		lock := r.prepareLockLocked(outputDir)
		r.mu.Unlock()
		// A check hung on a directory must not gain another blocked syscall
		// with every Start after a Stop; another directory is not held up.
		lock.Lock()
		var output *recordingOutput
		err := prepareRecordingDir(cfg.StorageMode, outputDir)
		if err == nil {
			nextRecordingFilename(base, func(name string) bool {
				path := filepath.Join(outputDir, name)
				// A ticket keeps its name, also while its file is missing.
				if _, exists := used[name]; exists || r.outbox.hasPath(path) {
					return true
				}
				output, err = r.reserveRecordingOutput(path, &cfg)
				return errors.Is(err, os.ErrExist)
			})
		}
		lock.Unlock()
		r.mu.Lock()
		if r.startGen == gen && r.state == state && r.config == cfg {
			return output, true, err
		}
		// Stale run or concurrent edit: never launch into the old path/name. The
		// cleanup keeps the directory lock, so a hung share holds one call at most.
		if output != nil {
			r.mu.Unlock()
			lock.Lock()
			output.discard()
			lock.Unlock()
			r.mu.Lock()
		}
	}
	return nil, false, nil
}

// recordingOutput keeps the exclusive reservation open through finalization,
// so a removed/replaced path cannot be mistaken for this encoder's file.
// Recording directories must be writable only by trusted operators: FFmpeg
// reopens the reserved path, so arbitrary hostile directory mutation is unsupported.
// Its ticket belongs to the recorder until the output is committed.
type recordingOutput struct {
	file   *os.File
	outbox *outbox
	ticket ticket
}

// reserveRecordingOutput exclusively creates path and its ticket, so the
// recording can be found after a crash from its first byte on.
func (r *GenericRecorder) reserveRecordingOutput(path string, cfg *types.Recorder) (*recordingOutput, error) {
	file, err := reserveRecordingFile(path)
	if err != nil {
		return nil, fmt.Errorf("reserve recording file: %w", err)
	}
	t := ticket{
		RecorderID:   cfg.ID,
		RecorderName: cfg.Name,
		Path:         path,
		Codec:        string(cfg.Codec),
	}
	if cfg.StorageMode != types.StorageLocal && cfg.HasS3Credentials() {
		t.Dest = destinationID(cfg.ID, cfg.S3Endpoint, cfg.S3Bucket)
		t.BaseKey = s3ObjectKey(cfg, filepath.Base(path))
		t.Key = t.BaseKey
		t.DeleteAfterUpload = cfg.StorageMode == types.StorageS3
		if err := r.outbox.ensureDestination(t.Dest, destinationOf(cfg)); err != nil {
			err = errors.Join(fmt.Errorf("save upload destination: %w", err), file.Close(), os.Remove(path)) // #nosec G703 -- The reservation this call created.
			return nil, err
		}
	} else if cfg.StorageMode != types.StorageLocal {
		slog.Warn("S3 not configured but storage mode requires it", "id", cfg.ID, "mode", cfg.StorageMode)
	}
	err = syncDir(filepath.Dir(path))
	if err == nil {
		err = r.outbox.newTicket(&t)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create recording ticket: %w", err), file.Close(), os.Remove(path)) // #nosec G703 -- The reservation this call created.
	}
	return &recordingOutput{file: file, outbox: r.outbox, ticket: t}, nil
}

func destinationOf(cfg *types.Recorder) destination {
	return destination{
		Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket,
		AccessKeyID: cfg.S3AccessKeyID, SecretAccessKey: cfg.S3SecretAccessKey,
	}
}

// recordFailure keeps the output for inspection; it is never uploaded.
func (o *recordingOutput) recordFailure(cause error) {
	o.outbox.fail(&o.ticket, cause)
}

// sync makes what the encoder wrote so far durable and records how much that
// is, so a restart after a power cut can tell lost audio from an empty file.
func (o *recordingOutput) sync() error {
	info, err := o.file.Stat()
	if err != nil {
		return err
	}
	if err := o.file.Sync(); err != nil {
		return err
	}
	if info.Size() <= o.ticket.SyncedBytes {
		return nil
	}
	o.ticket.SyncedBytes = info.Size()
	return o.outbox.update(&o.ticket)
}

// commit hands a finalized, released output on: in local-only mode the file is
// the recording and its ticket goes; otherwise the uploader takes the ticket.
func (o *recordingOutput) commit() error {
	if o.ticket.Dest == "" {
		return o.outbox.remove(&o.ticket)
	}
	o.ticket.State, o.ticket.ReadyAt = ticketReady, time.Now()
	return o.outbox.update(&o.ticket)
}

// release closes the reservation, once, before any upload or removal (Windows
// cannot remove an open file). It returns the output size if the path still
// names the file this recorder created.
func (o *recordingOutput) release() (int64, error) {
	defer func() {
		if err := o.file.Close(); err != nil {
			slog.Warn("failed to close recording reservation", "file", o.file.Name(), "error", err)
		}
	}()
	owned, err := o.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat recording output: %w", err)
	}
	info, err := os.Lstat(o.file.Name())
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("recording output disappeared before finalization: %w", err)
	}
	if err != nil {
		return 0, fmt.Errorf("check recording output ownership: %w", err)
	}
	if !os.SameFile(owned, info) {
		return 0, errors.New("recording output was replaced; refusing to upload or remove it")
	}
	return info.Size(), nil
}

// remove deletes a released output, then its ticket; one that is already gone
// counts as removed. When the removal fails, the ticket keeps the file out of uploads.
func (o *recordingOutput) remove() error {
	if err := removeEmptyRecording(o.file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty recording: %w", err)
	}
	return o.outbox.remove(&o.ticket)
}

// discardEmpty removes a released output without audio.
func (r *GenericRecorder) discardEmpty(run encoderRun) error {
	if err := run.output.remove(); err != nil {
		return err
	}
	slog.Info("discarded recording without audio", "id", r.id, "file", filepath.Base(run.file))
	return nil
}

// discard is for unstarted reservations; never remove an unrelated replacement.
func (o *recordingOutput) discard() {
	if o == nil {
		return
	}
	if _, err := o.release(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = o.outbox.remove(&o.ticket) // Nothing was recorded into it.
			return
		}
		o.recordFailure(err) // A replacement must not pass for a crash leftover.
		slog.Warn("cannot discard recording reservation", "file", o.file.Name(), "error", err)
		return
	}
	if err := o.remove(); err != nil {
		o.recordFailure(err)
		slog.Warn("failed to discard recording reservation", "file", o.file.Name(), "error", err)
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
		// The directories must survive a power cut with the audio in them,
		// also when an earlier attempt created them but could not sync them.
		err := mkdirSynced(dir, 0o755)
		if err == nil {
			err = errors.Join(syncDir(filepath.Dir(dir)), syncDir(filepath.Dir(filepath.Dir(dir))))
		}
		if err != nil {
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
func (r *GenericRecorder) startEncoderLocked(output *recordingOutput) error {
	codecArgs := types.BuildCodecArgs(r.config.Codec, r.config.Bitrate)
	format := r.config.Codec.Format()

	args := ffmpeg.BaseInputArgs()
	args = append(args, "-c:a")
	args = append(args, codecArgs...)
	args = append(args,
		"-f", format,
		"-flush_packets", "1", // Finished packets reach the kernel as they are muxed.
		"-hide_banner",
		"-loglevel", "warning",
		"-y", // This placeholder was exclusively created by this recorder.
		output.file.Name(),
	)

	result, err := ffmpeg.StartProcess(r.ffmpegPath, args)
	if err != nil {
		return err
	}

	r.currentFile, r.currentOutput = output.file.Name(), output
	r.rememberFilenameLocked(filepath.Base(r.currentFile))
	r.result = result
	r.stopSync = r.startSync(output)

	// Scope writer state to this FFmpeg process; rotation creates fresh channels.
	r.audioMu.Lock()
	r.audioCh = make(chan []byte, recorderAudioBufferChunks)
	r.audioMu.Unlock()
	r.writerDone = make(chan struct{})
	go r.audioWriter(result, r.audioCh, r.writerDone)

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(r.currentFile), "codec", r.config.Codec)

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
	output     *recordingOutput
	stopSync   func()
}

// startSync syncs output every syncInterval until the returned function, which
// waits for a sync in progress, is called. A failed sync is a recorder
// warning; the synced length does not advance.
func (r *GenericRecorder) startSync(output *recordingOutput) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			if err := output.sync(); err != nil {
				slog.Warn("cannot sync recording to disk", "id", r.id, "file", filepath.Base(output.file.Name()), "error", err)
				r.mu.Lock()
				if r.currentOutput == output {
					r.lastError = "cannot sync recording to disk: " + err.Error()
				}
				r.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// claimEncoderLocked detaches the active FFmpeg process so that the caller
// alone finishes it. Must be called with r.mu held.
func (r *GenericRecorder) claimEncoderLocked() encoderRun {
	run := encoderRun{
		result:     r.result,
		audioCh:    r.audioCh,
		writerDone: r.writerDone,
		file:       r.currentFile,
		output:     r.currentOutput,
		stopSync:   r.stopSync,
	}
	r.result = nil
	r.currentOutput = nil
	r.stopSync = nil
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
	run.stopSync() // Before the final sync and the release close the file.

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
		finalizeErr = errors.Join(fmt.Errorf("recording writer timed out after %s", writerDrainTimeout), r.drainProcess(run.result))
		<-run.writerDone
	}

	// writerDone has fired; remaining buffered chunks are teardown loss.
	if abandoned := len(run.audioCh); abandoned > 0 {
		drops := r.audioDrops.Add(int64(abandoned))
		slog.Warn("recorder abandoned buffered audio during teardown",
			"id", r.id, "file", filepath.Base(run.file), "chunks", abandoned, "total_drops", drops)
	}

	if finalizeErr != nil {
		r.failOutput(run, finalizeErr)
		return
	}
	// The recording is never handed on before its bytes are durable.
	if err := run.output.file.Sync(); err != nil {
		r.failOutput(run, fmt.Errorf("sync recording to disk: %w", err))
		return
	}
	if _, err := run.output.release(); err != nil {
		run.output.recordFailure(err)
		r.reportError(run.file, err)
		return
	}
	if run.result.BytesWritten() == 0 {
		if err := r.discardEmpty(run); err != nil {
			run.output.recordFailure(err)
			r.reportError(run.file, err)
		}
		return
	}
	if err := run.output.commit(); err != nil {
		// The ticket still says recording; the next start recovers it.
		r.reportError(run.file, fmt.Errorf("hand recording to upload: %w", err))
		return
	}
	if run.output.ticket.Dest == "" {
		slog.Info("recording saved", "id", r.id, "path", run.file)
		return
	}
	slog.Info("queued file for upload", "id", r.id, "file", filepath.Base(run.file))
	p := r.logParams()
	p.Filename, p.S3Key = filepath.Base(run.file), run.output.ticket.Key
	r.logEvent(eventlog.UploadQueued, p)
}

// logParams snapshots the recorder's event fields.
func (r *GenericRecorder) logParams() *eventlog.RecorderDetails {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.captureLogParamsLocked()
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

// rotateFile starts the next encoder before draining the previous one at an hour
// boundary. Bounded queues can still drop audio; AudioDrops counts lost chunks.
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
	output, current, prepareErr := r.prepareFilenameLocked(at)
	if !current || r.rotationGeneration != claimedGeneration {
		r.mu.Unlock()
		output.discard()
		return
	}
	if prepareErr != nil {
		r.scheduleRotationLocked(at)
		p := r.recordErrorLocked("", prepareErr)
		r.mu.Unlock()
		r.emitError(p, prepareErr)
		return
	}

	// Capture keeps feeding the current channel during preparation; only
	// the channel hand-over takes the separate audio lock.
	previous := r.claimEncoderLocked()
	r.retiring.Add(1)
	defer r.retiring.Done()
	startErr := r.startEncoderLocked(output)
	var failure *eventlog.RecorderDetails
	if startErr != nil {
		failure = r.recordErrorLocked(output.file.Name(), startErr)
		// The current encoder still runs: keep recording into its file instead
		// of stopping until the hourly retry.
		r.result, r.writerDone, r.currentOutput = previous.result, previous.writerDone, previous.output
		r.stopSync = previous.stopSync // The previous file keeps syncing.
		previous = encoderRun{}
	} else {
		r.lastError = "" // Clears the error of an earlier failed rotation.
		slog.Info("recorder rotated file", "id", r.id, "from", filepath.Base(previous.file), "to", filepath.Base(r.currentFile))
	}
	r.scheduleRotationLocked(at)
	r.mu.Unlock()
	if failure != nil {
		output.discard()
		r.emitError(failure, startErr)
	}

	r.finishEncoder(previous)
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
	return fmt.Sprintf("%s-%s.%s", r.config.RecordingFilePrefix(), t.Format(recordingTimestampLayout), r.config.Codec.FileExtension())
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

// s3Namespace is the object-key prefix for files under one filename prefix.
func s3Namespace(filePrefix string) string {
	return "recordings/" + filePrefix + "/"
}

func s3ObjectKey(recorder *types.Recorder, filename string) string {
	return s3Namespace(recorder.RecordingFilePrefix()) + filename
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
