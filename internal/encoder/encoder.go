// Package encoder provides the audio capture and encoding engine.
// It manages real-time PCM audio distribution to multiple FFmpeg stream
// processes, with automatic retry, silence detection, and level metering.
package encoder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const (
	// LevelUpdateSamples is the sample count between level updates.
	LevelUpdateSamples = 12000
	// distributorBufferSize holds ~100ms of PCM audio per read.
	distributorBufferSize = audio.BytesPerSecond / 10
)

// ErrNoAudioInput is returned when no audio input device is configured.
var ErrNoAudioInput = errors.New("no audio input configured")

// ErrAlreadyRunning is returned when the encoder is already running.
var ErrAlreadyRunning = errors.New("encoder already running")

// ErrNotRunning is returned when the encoder is not running.
var ErrNotRunning = errors.New("encoder not running")

// ErrStreamDisabled is returned when the stream is disabled.
var ErrStreamDisabled = errors.New("stream is disabled")

// ErrStreamNotFound is returned when the stream was not found.
var ErrStreamNotFound = errors.New("stream not found")

// ErrSRTUnsupported is returned when the FFmpeg build lacks SRT protocol support.
var ErrSRTUnsupported = errors.New("ffmpeg does not support srt protocol")

// ErrRecordingNotAvailable is returned when the recording manager is not initialized.
var ErrRecordingNotAvailable = errors.New("recording not available")

// Encoder is the audio capture and distribution engine.
type Encoder struct {
	config              *config.Config
	ffmpegPath          string
	buildCaptureCommand func(device, ffmpegPath string) (string, []string, error)
	streamRestartDelay  time.Duration
	srtAvailable        bool
	srtProbeError       error
	streamManager       *streaming.Manager
	recordingManager    *recording.Manager
	silenceDumpManager  *silencedump.Manager
	eventLogger         *eventlog.Logger
	sourceCmd           *exec.Cmd
	sourceCancel        context.CancelFunc
	sourceStdout        io.ReadCloser
	sourceRunID         uint64
	state               types.EncoderState
	stopChan            chan struct{}
	mu                  sync.RWMutex
	closeOnce           sync.Once
	closeErr            error
	lastError           string
	startTime           time.Time
	retryCount          int
	backoff             *util.Backoff
	audioLevels         atomic.Pointer[audio.AudioLevels] // published lock-free; see AudioLevels
	silenceDetect       *audio.SilenceDetector
	imbalanceDetect     *audio.ImbalanceDetector
	alertOrchestrator   *notify.AlertOrchestrator
	peakHolder          *audio.PeakHolder
	secretExpiryChecker *notify.SecretExpiryChecker
}

// New creates a new Encoder with the given configuration and FFmpeg binary path.
func New(cfg *config.Config, ffmpegPath string) (*Encoder, error) {
	graphCfg := cfg.GraphConfig()
	snap := cfg.Snapshot()

	webhookCh := &notify.WebhookChannel{}
	emailCh := &notify.EmailChannel{}
	zabbixCh := &notify.ZabbixChannel{}
	dispatcher := notify.NewDispatcher(webhookCh, emailCh, zabbixCh)
	orchestrator := notify.NewAlertOrchestrator(cfg, dispatcher)

	// Create dump manager with callback to alert orchestrator
	dumpManager := silencedump.NewManager(
		ffmpegPath,
		snap.WebPort,
		snap.SilenceDumpEnabled,
		snap.SilenceDumpRetentionDays,
		orchestrator.OnDumpReady,
	)

	// Create event logger with platform-specific path
	eventLogPath := eventlog.DefaultLogPath(snap.WebPort)
	logger, err := eventlog.NewLogger(eventLogPath)
	if err != nil {
		return nil, fmt.Errorf("create event logger at %s: %w", eventLogPath, err)
	}

	// Wire event logger to alert orchestrator for silence event logging
	orchestrator.SetEventLogger(logger)

	// Create stream manager and wire up event callback
	streamMgr := streaming.NewManager(ffmpegPath)
	srtAvailable, srtProbeErr := util.ProbeFFmpegProtocol(ffmpegPath, "srt")
	switch {
	case srtProbeErr != nil:
		slog.Warn("could not verify FFmpeg SRT protocol support; SRT streams will still be attempted",
			"path", ffmpegPath, "error", srtProbeErr)
	case ffmpegPath != "" && !srtAvailable:
		slog.Warn("FFmpeg found but SRT protocol is not available", "path", ffmpegPath)
	}

	e := &Encoder{
		config:              cfg,
		ffmpegPath:          ffmpegPath,
		buildCaptureCommand: audio.BuildCaptureCommand,
		streamRestartDelay:  types.StreamRestartDelay,
		srtAvailable:        srtAvailable,
		srtProbeError:       srtProbeErr,
		streamManager:       streamMgr,
		silenceDumpManager:  dumpManager,
		eventLogger:         logger,
		state:               types.StateStopped,
		backoff:             util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay),
		silenceDetect:       audio.NewSilenceDetector(),
		imbalanceDetect:     audio.NewImbalanceDetector(),
		alertOrchestrator:   orchestrator,
		peakHolder:          audio.NewPeakHolder(),
		secretExpiryChecker: notify.NewSecretExpiryChecker(&graphCfg),
	}

	// Set event callback on stream manager
	streamMgr.SetEventCallback(e.onStreamEvent, e.getStreamName)

	return e, nil
}

// SRTAvailable reports whether the configured FFmpeg binary supports SRT.
func (e *Encoder) SRTAvailable() bool {
	if e == nil {
		return false
	}
	// A failed probe is inconclusive, not proof that SRT is unavailable. Keep
	// SRT usable and let the real FFmpeg stream command report a definitive
	// protocol error if necessary.
	return e.srtAvailable || (e.ffmpegPath != "" && e.srtProbeError != nil)
}

// srtCapabilityError reports why SRT streams cannot run, or nil when SRT is usable
// or unconfigured. A probe error is inconclusive, so it also returns nil and lets
// the real stream process determine whether SRT works.
func (e *Encoder) srtCapabilityError() error {
	if e == nil || e.ffmpegPath == "" || e.SRTAvailable() {
		return nil
	}
	return ErrSRTUnsupported
}

// SRTErrorMessage returns the user-facing SRT capability error, if any.
func (e *Encoder) SRTErrorMessage() string {
	if err := e.srtCapabilityError(); err != nil {
		return err.Error()
	}
	return ""
}

func (e *Encoder) onStreamEvent(
	streamID, streamName, mode, eventType, message, errMsg string,
	retryCount, maxRetries int,
) {
	if e.eventLogger == nil {
		return
	}

	if err := e.eventLogger.LogStream(
		eventlog.EventType(eventType), streamID, streamName, mode, message, errMsg,
		retryCount, maxRetries,
	); err != nil {
		slog.Warn("failed to log stream event", "error", err)
	}
}

func (e *Encoder) getStreamName(streamID string) string {
	stream := e.config.Stream(streamID)
	if stream == nil {
		return ""
	}
	return stream.Endpoint()
}

// EventLogPath returns the path to the event log file.
func (e *Encoder) EventLogPath() string {
	if e.eventLogger == nil {
		return ""
	}
	return e.eventLogger.Path()
}

// EventSeq returns the event log's change counter, which increments on every
// logged event and is zero when no logger is configured. Live views compare
// successive values to detect new events without refetching on every status tick.
func (e *Encoder) EventSeq() uint64 {
	if e.eventLogger == nil {
		return 0
	}
	return e.eventLogger.Seq()
}

// InitRecording prepares the recording manager for use.
func (e *Encoder) InitRecording() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap := e.config.Snapshot()

	mgr, err := recording.NewManager(
		e.ffmpegPath,
		recording.DefaultSpoolDir(),
		snap.RecordingMaxDurationMinutes,
		e.eventLogger,
	)
	if err != nil {
		return fmt.Errorf("create recording manager: %w", err)
	}

	// Wire upload abandoned callback to dispatch to all configured notification channels
	mgr.SetUploadAbandonedCallback(func(event recording.UploadAbandonedEvent) {
		e.alertOrchestrator.HandleUploadAbandoned(notify.UploadAbandonedData{
			RecorderName: event.RecorderName,
			Filename:     event.Filename,
			S3Key:        event.S3Key,
			LastError:    event.LastError,
			RetryCount:   event.RetryCount,
		})
	})

	// Add all configured recorders
	for i := range snap.Recorders {
		if err := mgr.AddRecorder(&snap.Recorders[i]); err != nil {
			slog.Warn("failed to add recorder", "id", snap.Recorders[i].ID, "error", err)
		}
	}

	e.recordingManager = mgr
	return nil
}

// RecordingAvailable reports whether the recording manager is initialized.
func (e *Encoder) RecordingAvailable() bool {
	return e.recordingManager != nil
}

// RecorderStatuses returns status for all configured recorders.
func (e *Encoder) RecorderStatuses() map[string]types.ProcessStatus {
	if e.recordingManager == nil {
		return map[string]types.ProcessStatus{}
	}
	return e.recordingManager.Statuses()
}

// PendingUploadCount returns the number of recording uploads waiting for retry.
func (e *Encoder) PendingUploadCount() int {
	if e.recordingManager == nil {
		return 0
	}
	return e.recordingManager.PendingUploadCount()
}

// State returns the current encoder state.
func (e *Encoder) State() types.EncoderState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Stream returns the stream configuration for the given ID.
func (e *Encoder) Stream(streamID string) *types.Stream {
	return e.config.Stream(streamID)
}

// IsRunning reports whether the encoder is in running state.
func (e *Encoder) IsRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == types.StateRunning
}

// silentAudioLevels is reported before live metering is available.
// -60 dB is the meter floor.
var silentAudioLevels = audio.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}

// AudioLevels returns the most recently published audio levels.
//
// Reads use an atomic pointer so the UI cannot block distributor writes. A nil
// pointer reads as silence.
func (e *Encoder) AudioLevels() audio.AudioLevels {
	if levels := e.audioLevels.Load(); levels != nil {
		return *levels
	}
	return silentAudioLevels
}

// Status returns the current encoder status.
func (e *Encoder) Status() types.EncoderStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var uptime string
	var uptimeSeconds int64
	if e.state == types.StateRunning {
		d := time.Since(e.startTime).Truncate(time.Second)
		uptime = d.String()
		uptimeSeconds = int64(d.Seconds())
	}

	return types.EncoderStatus{
		State:            e.state,
		Uptime:           uptime,
		UptimeSeconds:    uptimeSeconds,
		LastError:        e.lastError,
		SourceRetryCount: e.retryCount,
		SourceMaxRetries: types.MaxRetries,
	}
}

// StreamStatuses returns status for all configured streams.
func (e *Encoder) StreamStatuses(streams []types.Stream) map[string]types.ProcessStatus {
	// Get statuses for streams with active processes
	processStatuses := e.streamManager.Statuses(e.config.Stream)

	// Build complete status map for all configured streams
	result := make(map[string]types.ProcessStatus, len(streams))
	for i := range streams {
		stream := &streams[i]
		status, exists := processStatuses[stream.ID]
		switch {
		case exists:
			// Stream has active process - use its status
			// Also reflect disabled state if stream was disabled while running
			if !stream.Enabled {
				status.State = types.ProcessDisabled
			}
			result[stream.ID] = status
		case !stream.Enabled:
			// Stream is disabled - mark explicitly
			result[stream.ID] = types.ProcessStatus{
				State:      types.ProcessDisabled,
				MaxRetries: stream.MaxRetriesOrDefault(),
			}
		case stream.RequiresFFmpegSRT() && e.srtCapabilityError() != nil:
			result[stream.ID] = types.ProcessStatus{
				State:      types.ProcessError,
				MaxRetries: stream.MaxRetriesOrDefault(),
				Error:      e.SRTErrorMessage(),
			}
		default:
			// Stream is enabled but has no process (encoder not running)
			result[stream.ID] = types.ProcessStatus{
				State:      types.ProcessStopped,
				MaxRetries: stream.MaxRetriesOrDefault(),
			}
		}
	}
	return result
}

// Start starts audio capture and stream processes.
func (e *Encoder) Start() error {
	if e.config.AudioInput() == "" {
		return ErrNoAudioInput
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == types.StateRunning || e.state == types.StateStarting || e.state == types.StateStopping {
		return ErrAlreadyRunning
	}

	e.state = types.StateStarting
	e.stopChan = make(chan struct{})
	e.retryCount = 0
	e.backoff.Reset()
	e.silenceDetect.Reset()
	e.imbalanceDetect.Reset()
	e.alertOrchestrator.Reset()
	e.peakHolder.Reset()

	go e.runSourceLoop()

	return nil
}

// Stop shuts down all processes.
func (e *Encoder) Stop() error {
	e.mu.Lock()

	if e.state == types.StateStopped || e.state == types.StateStopping {
		e.mu.Unlock()
		return nil
	}

	e.state = types.StateStopping

	if e.stopChan != nil {
		close(e.stopChan)
	}

	// Get references while holding lock
	sourceProcess := e.sourceCmd
	sourceCancel := e.sourceCancel
	e.mu.Unlock()

	// Stop all streams and recording first; collect all shutdown errors.
	errs := e.stopConsumers()

	// Send graceful termination signal to source.
	if sourceProcess != nil && sourceProcess.Process != nil {
		if err := util.GracefulSignal(sourceProcess.Process); err != nil {
			slog.Warn("failed to send signal to source", "error", err)
			errs = append(errs, fmt.Errorf("signal source: %w", err))
		}
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()

	stopped := e.pollUntil(pollCtx, func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.sourceCmd == nil
	})

	select {
	case <-stopped:
		slog.Info("source capture stopped gracefully")
	case <-time.After(types.ShutdownTimeout):
		pollCancel() // Stop polling goroutine immediately
		slog.Warn("source capture did not stop in time, forcing kill")
		if sourceCancel != nil {
			sourceCancel()
		}
		errs = append(errs, fmt.Errorf("source shutdown timeout"))
	}

	e.resetDetection()

	e.mu.Lock()
	e.state = types.StateStopped
	e.sourceCmd = nil
	e.sourceCancel = nil
	e.mu.Unlock()

	return errors.Join(errs...)
}

// stopConsumers stops the stream and recorder output processes and returns
// any shutdown errors. Shared by Stop and the source-retry give-up path.
// (Compliance recording continues independently of the recording manager.)
func (e *Encoder) stopConsumers() []error {
	var errs []error
	if err := e.streamManager.StopAll(); err != nil {
		errs = append(errs, fmt.Errorf("stop streams: %w", err))
	}
	if e.recordingManager != nil {
		if err := e.recordingManager.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop recording: %w", err))
		}
	}
	return errs
}

// resetDetection clears silence/imbalance detection and notification state,
// stops the silence dump manager, and drains queued alert log entries.
// Shared by Stop and the source-retry give-up path.
func (e *Encoder) resetDetection() {
	e.silenceDetect.Reset()
	e.imbalanceDetect.Reset()
	e.alertOrchestrator.Reset()

	if e.silenceDumpManager != nil {
		e.silenceDumpManager.Stop()
	}

	e.alertOrchestrator.DrainLogs()
}

// Restart stops and restarts the encoder.
func (e *Encoder) Restart() error {
	if err := e.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	time.Sleep(1 * time.Second)
	return e.Start()
}

// Close releases process-lifetime resources. Callers should stop the encoder first.
func (e *Encoder) Close() error {
	e.closeOnce.Do(func() {
		if e.alertOrchestrator != nil {
			e.alertOrchestrator.Close()
		}
		if e.eventLogger != nil {
			e.closeErr = e.eventLogger.Close()
		}
	})
	return e.closeErr
}

// StartStream initiates a streaming process.
func (e *Encoder) StartStream(streamID string) error {
	return e.startStream(streamID, 0)
}

// RestartStream stops a stream and schedules its start after
// types.StreamRestartDelay. The delayed start is bound to the current source
// run, so a stale restarter cannot revive the stream after the encoder
// stopped or restarted in the meantime. No-op when the encoder is not running.
func (e *Encoder) RestartStream(streamID string) {
	e.mu.RLock()
	running := e.state == types.StateRunning
	runID := e.sourceRunID
	stopChan := e.stopChan
	e.mu.RUnlock()
	if !running {
		return
	}

	if err := e.StopStream(streamID); err != nil {
		slog.Warn("failed to stop stream for restart", "stream_id", streamID, "error", err)
	}

	go func() {
		timer := time.NewTimer(types.StreamRestartDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stopChan:
			return
		}
		if !e.sourceRunActive(runID) {
			return
		}
		if err := e.startStream(streamID, runID); err != nil {
			slog.Warn("failed to restart stream", "stream_id", streamID, "error", err)
		}
	}()
}

// ApplySettingsEffects applies the runtime side effects of a settings save:
// detector resets, silence dump and recording limits, and Graph secret expiry
// cache invalidation. Keeping the list here means a new settings consumer only
// needs a change in this method, not in the HTTP layer.
func (e *Encoder) ApplySettingsEffects() {
	e.UpdateSilenceConfig()
	e.UpdateChannelImbalanceConfig()
	e.UpdateSilenceDumpConfig()
	e.UpdateRecordingMaxDuration()
	e.InvalidateGraphSecretExpiryCache()
}

// startStream starts a stream, optionally bound to a specific source run.
// A runID of 0 means "no ownership constraint" (manual API calls); a non-zero
// runID only starts the stream while that source run is still active, so a stale
// delayed starter cannot revive streams after the source has exited or restarted.
func (e *Encoder) startStream(streamID string, runID uint64) error {
	var stopChan chan struct{}

	e.mu.RLock()
	activeRun := runID == 0 || e.sourceRunID == runID
	if e.state != types.StateRunning || !activeRun {
		e.mu.RUnlock()
		return ErrNotRunning
	}
	stopChan = e.stopChan
	e.mu.RUnlock()

	stream := e.config.Stream(streamID)
	if stream == nil {
		return ErrStreamNotFound
	}
	if !stream.Enabled {
		return ErrStreamDisabled
	}
	if stream.RequiresFFmpegSRT() {
		if err := e.srtCapabilityError(); err != nil {
			return err
		}
	}

	// Start preserves existing retry state automatically
	started, err := e.streamManager.Start(stream)
	if err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	// Only a new process owns a retry monitor.
	if started {
		go e.streamManager.MonitorAndRetry(streamID, e, stopChan)
	}

	return nil
}

// StopStream terminates the stream with the given ID.
func (e *Encoder) StopStream(streamID string) error {
	return e.streamManager.Stop(streamID)
}

// GraphSecretExpiry returns the current Graph API client secret expiry info.
func (e *Encoder) GraphSecretExpiry() types.SecretExpiryInfo {
	if e.secretExpiryChecker == nil {
		return types.SecretExpiryInfo{}
	}
	return e.secretExpiryChecker.Info()
}

// InvalidateGraphSecretExpiryCache clears the cached secret expiry info.
func (e *Encoder) InvalidateGraphSecretExpiryCache() {
	if e.secretExpiryChecker != nil {
		graphCfg := e.config.GraphConfig()
		e.secretExpiryChecker.UpdateConfig(&graphCfg)
	}
}

// UpdateSilenceConfig resets silence detection after config changes.
func (e *Encoder) UpdateSilenceConfig() {
	if e.silenceDetect != nil {
		e.silenceDetect.Reset()
	}
}

// UpdateChannelImbalanceConfig resets imbalance timing after detector settings change.
// It also runs when the silence threshold changes because that threshold is the
// presence floor.
func (e *Encoder) UpdateChannelImbalanceConfig() {
	if e.imbalanceDetect != nil {
		e.imbalanceDetect.Reset()
	}
}

// UpdateRecordingMaxDuration applies the current max duration setting to the recording manager.
func (e *Encoder) UpdateRecordingMaxDuration() {
	if e.recordingManager == nil {
		return
	}
	snap := e.config.Snapshot()
	e.recordingManager.SetMaxDurationMinutes(snap.RecordingMaxDurationMinutes)
}

// UpdateSilenceDumpConfig applies the current silence dump configuration.
func (e *Encoder) UpdateSilenceDumpConfig() {
	snap := e.config.Snapshot()
	if e.silenceDumpManager != nil {
		e.silenceDumpManager.SetEnabled(snap.SilenceDumpEnabled)
		e.silenceDumpManager.SetRetentionDays(snap.SilenceDumpRetentionDays)
	}
}

// isStopping reports whether shutdown has begun. The caller must hold e.mu.
func (e *Encoder) isStopping() bool {
	return e.state == types.StateStopping || e.state == types.StateStopped
}

// runSourceLoop manages the audio capture lifecycle with automatic retry and backoff.
func (e *Encoder) runSourceLoop() {
	for {
		e.mu.Lock()
		if e.isStopping() {
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()

		startTime := time.Now()
		stderrOutput, err := e.runSource()
		runDuration := time.Since(startTime)

		e.mu.Lock()
		if e.isStopping() {
			e.mu.Unlock()
			return
		}

		if err != nil {
			errMsg := err.Error()
			if stderrOutput != "" {
				errMsg = stderrOutput
			}
			e.lastError = errMsg
			slog.Error("source capture error", "error", errMsg)

			if runDuration >= types.SuccessThreshold {
				e.retryCount = 0
				e.backoff.Reset()
			} else {
				e.retryCount++
			}

			if e.retryCount >= types.MaxRetries {
				slog.Error("source capture failed, giving up", "attempts", types.MaxRetries)
				e.state = types.StateStopping
				e.lastError = fmt.Sprintf("Stopped after %d failed attempts: %s", types.MaxRetries, errMsg)
				e.mu.Unlock()
				for _, stopErr := range e.stopConsumers() {
					slog.Error("failed to stop consumers during source failure", "error", stopErr)
				}
				e.resetDetection()
				e.mu.Lock()
				e.state = types.StateStopped
				e.mu.Unlock()
				return
			}
		}

		if err == nil {
			e.retryCount = 0
			e.backoff.Reset()
		}

		if e.isStopping() {
			e.mu.Unlock()
			return
		}

		e.state = types.StateStarting
		retryDelay := e.backoff.Next()
		e.mu.Unlock()

		slog.Info("source stopped, waiting before restart",
			"delay", retryDelay, "attempt", e.retryCount+1, "max_retries", types.MaxRetries)
		select {
		case <-e.stopChan:
			return
		case <-time.After(retryDelay):
		}
	}
}

// runSource starts the audio capture process and blocks until it exits.
func (e *Encoder) runSource() (string, error) {
	audioInput := e.config.Snapshot().AudioInput
	cmdName, args, err := e.buildCaptureCommand(audioInput, e.ffmpegPath)
	if err != nil {
		return "", err
	}

	slog.Info("starting audio capture", "command", cmdName, "input", audioInput)

	ctx, cancel := context.WithCancel(context.Background())
	//nolint:gosec // cmdName is from internal platform config (arecord/ffmpeg)
	cmd := exec.CommandContext(ctx, cmdName, args...)

	// Go 1.20+: Declarative graceful shutdown - sends signal first, waits, then kills.
	cmd.Cancel = func() error {
		return util.GracefulSignal(cmd.Process)
	}
	cmd.WaitDelay = types.ShutdownTimeout

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", err
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}

	runID, stopChan, published := e.publishSource(cmd, cancel, stdoutPipe)
	if !published {
		cancel()
		err := cmd.Wait()
		return util.ExtractLastError(stderrBuf.String()), err
	}

	e.resetAudioLevels() // start each run silent until the first metered chunk

	go e.startEnabledStreamsAfterDelay(runID, stopChan, e.streamRestartDelay)

	err = cmd.Wait()

	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.sourceCmd = nil
		e.sourceCancel = nil
		e.sourceStdout = nil
	}()
	// runDistributor publishes final silence after its last live level.

	return util.ExtractLastError(stderrBuf.String()), err
}

// publishSource records a freshly started capture process as the active source
// run and returns its run ID and stop channel. It returns ok=false without
// recording anything if shutdown began before the process could be published, so
// the caller can tear the process back down.
func (e *Encoder) publishSource(
	cmd *exec.Cmd,
	cancel context.CancelFunc,
	stdout io.ReadCloser,
) (runID uint64, stopChan <-chan struct{}, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.isStopping() {
		return 0, nil, false
	}
	e.sourceRunID++
	e.sourceCmd = cmd
	e.sourceCancel = cancel
	e.sourceStdout = stdout
	e.state = types.StateRunning
	e.startTime = time.Now()
	e.lastError = ""
	return e.sourceRunID, e.stopChan, true
}

func (e *Encoder) startEnabledStreamsAfterDelay(runID uint64, stopChan <-chan struct{}, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		e.startEnabledStreams(runID)
	case <-stopChan:
	}
}

func (e *Encoder) sourceRunActive(runID uint64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sourceRunID == runID && e.state == types.StateRunning && e.sourceStdout != nil
}

func (e *Encoder) startEnabledStreams(runID uint64) {
	if !e.sourceRunActive(runID) {
		return
	}

	// Start silence dump manager (cleanup scheduler)
	if e.silenceDumpManager != nil {
		e.silenceDumpManager.Start()
	}

	if !e.sourceRunActive(runID) {
		return
	}

	go e.runDistributor(runID)

	streams := e.config.ConfiguredStreams()
	for i := range streams {
		if !e.sourceRunActive(runID) {
			return
		}
		stream := &streams[i]
		if !stream.Enabled {
			slog.Info("skipping disabled stream", "stream_id", stream.ID)
			continue
		}
		if err := e.startStream(stream.ID, runID); err != nil {
			slog.Error("failed to start stream", "stream_id", stream.ID, "error", err)
		}
	}

	if !e.sourceRunActive(runID) {
		return
	}

	// Start recording manager (starts auto-start recorders)
	if e.recordingManager != nil {
		if err := e.recordingManager.Start(); err != nil {
			slog.Error("failed to start recording manager", "error", err)
		}
	}
}

// runDistributor reads PCM audio and fans it out to meters, alerts, streams, and recorders.
func (e *Encoder) runDistributor(runID uint64) {
	buf := make([]byte, distributorBufferSize)

	distributor := NewDistributor(DistributorConfig{
		SilenceDetect:      e.silenceDetect,
		ImbalanceDetect:    e.imbalanceDetect,
		AlertOrchestrator:  e.alertOrchestrator,
		SilenceDumpManager: e.silenceDumpManager,
		PeakHolder:         e.peakHolder,
		Config:             e.config,
		Callback:           e.updateAudioLevels,
	})

	// Publish silence from this goroutine so it wins after the last live level.
	defer e.resetAudioLevels()

	for {
		e.mu.RLock()
		state := e.state
		reader := e.sourceStdout
		stopChan := e.stopChan
		activeRun := e.sourceRunID == runID
		e.mu.RUnlock()

		if !activeRun || state != types.StateRunning || reader == nil {
			return
		}

		select {
		case <-stopChan:
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		chunk := buf[:n]

		// ProcessSamples also feeds the silence dump ring buffer.
		distributor.ProcessSamples(chunk)

		// One lazy clone shared by the stream and recorder queues: the capture
		// buffer is reused, so consumers get an immutable copy, but never more
		// than one per chunk. Cloning is skipped entirely when nothing can
		// receive audio. All queue consumers treat chunks as read-only.
		var shared []byte
		copyChunk := func() []byte {
			if shared == nil {
				shared = bytes.Clone(chunk)
			}
			return shared
		}
		e.streamManager.WriteAudioFanOutShared(copyChunk)

		// Recording fan-out stays non-blocking; recorders own their queues.
		if e.recordingManager != nil {
			e.recordingManager.WriteAudioShared(copyChunk)
		}
	}
}

// updateAudioLevels publishes a freshly allocated, immutable snapshot.
func (e *Encoder) updateAudioLevels(levels *audio.AudioLevels) {
	e.audioLevels.Store(levels)
}

// resetAudioLevels publishes the silent snapshot used when no audio is flowing.
func (e *Encoder) resetAudioLevels() {
	levels := silentAudioLevels
	e.audioLevels.Store(&levels)
}

// pollUntil signals when the given condition becomes true.
// The goroutine exits when either the condition is met or the context is cancelled.
func (e *Encoder) pollUntil(ctx context.Context, condition func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(types.PollInterval)
		defer ticker.Stop()
		for !condition() {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// AddRecorder creates a new recorder.
func (e *Encoder) AddRecorder(cfg *types.Recorder) error {
	if e.recordingManager == nil {
		return ErrRecordingNotAvailable
	}
	if err := e.config.AddRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.AddRecorder(cfg)
}

// RemoveRecorder deletes a recorder.
func (e *Encoder) RemoveRecorder(id string) error {
	if e.recordingManager == nil {
		return ErrRecordingNotAvailable
	}
	if err := e.recordingManager.RemoveRecorder(id); err != nil {
		slog.Warn("error removing recorder from manager", "id", id, "error", err)
	}
	return e.config.RemoveRecorder(id)
}

// UpdateRecorder modifies a recorder configuration.
func (e *Encoder) UpdateRecorder(cfg *types.Recorder) error {
	if e.recordingManager == nil {
		return ErrRecordingNotAvailable
	}
	if err := e.config.UpdateRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.UpdateRecorder(cfg)
}

// StartRecorder initiates a recorder.
func (e *Encoder) StartRecorder(id string) error {
	if e.recordingManager == nil {
		return ErrRecordingNotAvailable
	}
	return e.recordingManager.StartRecorder(id)
}

// StopRecorder terminates a recorder.
func (e *Encoder) StopRecorder(id string) error {
	if e.recordingManager == nil {
		return ErrRecordingNotAvailable
	}
	return e.recordingManager.StopRecorder(id)
}
