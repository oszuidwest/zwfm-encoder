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

// LevelUpdateSamples is the number of samples to process between level updates.
const LevelUpdateSamples = 12000

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

// Encoder is the audio capture and distribution engine.
type Encoder struct {
	config              *config.Config
	ffmpegPath          string
	streamManager       *streaming.Manager
	recordingManager    *recording.Manager
	silenceDumpManager  *silencedump.Manager
	eventLogger         *eventlog.Logger
	sourceCmd           *exec.Cmd
	sourceCancel        context.CancelFunc
	sourceStdout        io.ReadCloser
	state               types.EncoderState
	stopChan            chan struct{}
	mu                  sync.RWMutex
	lastError           string
	startTime           time.Time
	retryCount          int
	backoff             *util.Backoff
	audioLevels         audio.AudioLevels
	lastKnownLevels     audio.AudioLevels // Cache for TryRLock fallback
	silenceDetect       *audio.SilenceDetector
	silenceNotifier     *notify.SilenceNotifier
	peakHolder          *audio.PeakHolder
	secretExpiryChecker *notify.SecretExpiryChecker
}

// New creates a new Encoder with the given configuration and FFmpeg binary path.
func New(cfg *config.Config, ffmpegPath string) (*Encoder, error) {
	graphCfg := cfg.GraphConfig()
	snap := cfg.Snapshot()

	// Create notifier first (no dependencies)
	notifier := notify.NewSilenceNotifier(cfg)

	// Create dump manager with callback to notifier
	dumpManager := silencedump.NewManager(
		ffmpegPath,
		snap.WebPort,
		snap.SilenceDumpEnabled,
		snap.SilenceDumpRetentionDays,
		notifier.OnDumpReady,
	)

	// Create event logger with platform-specific path
	eventLogPath := eventlog.DefaultLogPath(snap.WebPort)
	logger, err := eventlog.NewLogger(eventLogPath)
	if err != nil {
		return nil, fmt.Errorf("create event logger at %s: %w", eventLogPath, err)
	}

	// Wire event logger to notifier for silence event logging
	notifier.SetEventLogger(logger)

	// Create stream manager and wire up event callback
	streamMgr := streaming.NewManager(ffmpegPath)

	e := &Encoder{
		config:              cfg,
		ffmpegPath:          ffmpegPath,
		streamManager:       streamMgr,
		silenceDumpManager:  dumpManager,
		eventLogger:         logger,
		state:               types.StateStopped,
		backoff:             util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay),
		silenceDetect:       audio.NewSilenceDetector(),
		silenceNotifier:     notifier,
		peakHolder:          audio.NewPeakHolder(),
		secretExpiryChecker: notify.NewSecretExpiryChecker(&graphCfg),
	}

	// Set event callback on stream manager
	streamMgr.SetEventCallback(e.onStreamEvent, e.getStreamName)

	return e, nil
}

// onStreamEvent handles stream events from the streaming manager.
func (e *Encoder) onStreamEvent(streamID, streamName, eventType, message, errMsg string, retryCount, maxRetries int) {
	if e.eventLogger == nil {
		return
	}

	if err := e.eventLogger.LogStream(eventlog.EventType(eventType), streamID, streamName, message, errMsg, retryCount, maxRetries); err != nil {
		slog.Warn("failed to log stream event", "error", err)
	}
}

// getStreamName returns a display name for a stream by ID.
func (e *Encoder) getStreamName(streamID string) string {
	stream := e.config.Stream(streamID)
	if stream == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", stream.Host, stream.Port)
}

// EventLogPath returns the path to the event log file.
func (e *Encoder) EventLogPath() string {
	if e.eventLogger == nil {
		return ""
	}
	return e.eventLogger.Path()
}

// InitRecording prepares the recording manager for use.
func (e *Encoder) InitRecording() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap := e.config.Snapshot()

	mgr, err := recording.NewManager(e.ffmpegPath, "", snap.RecordingMaxDurationMinutes, e.eventLogger)
	if err != nil {
		return fmt.Errorf("create recording manager: %w", err)
	}

	// Add all configured recorders
	for i := range snap.Recorders {
		if err := mgr.AddRecorder(&snap.Recorders[i]); err != nil {
			slog.Warn("failed to add recorder", "id", snap.Recorders[i].ID, "error", err)
		}
	}

	e.recordingManager = mgr
	return nil
}

// AllRecorderStatuses returns status for all configured recorders.
func (e *Encoder) AllRecorderStatuses() map[string]types.ProcessStatus {
	return e.recordingManager.AllStatuses()
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

// AudioLevels returns the current audio levels.
func (e *Encoder) AudioLevels() audio.AudioLevels {
	if !e.mu.TryRLock() {
		return e.lastKnownLevels
	}
	defer e.mu.RUnlock()

	if e.state != types.StateRunning {
		return audio.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}
	}
	return e.audioLevels
}

// Status returns the current encoder status.
func (e *Encoder) Status() types.EncoderStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	uptime := ""
	if e.state == types.StateRunning {
		uptime = time.Since(e.startTime).Truncate(time.Second).String()
	}

	return types.EncoderStatus{
		State:            e.state,
		Uptime:           uptime,
		LastError:        e.lastError,
		SourceRetryCount: e.retryCount,
		SourceMaxRetries: types.MaxRetries,
	}
}

// AllStreamStatuses returns status for all configured streams.
func (e *Encoder) AllStreamStatuses(streams []types.Stream) map[string]types.ProcessStatus {
	// Get statuses for streams with active processes
	processStatuses := e.streamManager.AllStatuses(func(id string) int {
		if s := e.config.Stream(id); s != nil {
			return s.MaxRetriesOrDefault()
		}
		return types.DefaultMaxRetries
	})

	// Build complete status map for all configured streams
	result := make(map[string]types.ProcessStatus, len(streams))
	for _, stream := range streams {
		if status, exists := processStatuses[stream.ID]; exists {
			// Stream has active process - use its status
			// Also reflect disabled state if stream was disabled while running
			if !stream.IsEnabled() {
				status.State = types.ProcessDisabled
			}
			result[stream.ID] = status
		} else if !stream.IsEnabled() {
			// Stream is disabled - mark explicitly
			result[stream.ID] = types.ProcessStatus{
				State:      types.ProcessDisabled,
				MaxRetries: stream.MaxRetriesOrDefault(),
			}
		} else {
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

	if e.state == types.StateRunning || e.state == types.StateStarting {
		return ErrAlreadyRunning
	}

	e.state = types.StateStarting
	e.stopChan = make(chan struct{})
	e.retryCount = 0
	e.backoff.Reset()
	e.silenceDetect.Reset()
	e.silenceNotifier.Reset()
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

	// Collect all shutdown errors
	var errs []error

	// Stop all streams first
	if err := e.streamManager.StopAll(); err != nil {
		errs = append(errs, fmt.Errorf("stop streams: %w", err))
	}

	// Stop recording manager (note: compliance recording continues independently)
	if err := e.recordingManager.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("stop recording: %w", err))
	}

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

	// Reset silence detection and notification state
	e.silenceDetect.Reset()
	e.silenceNotifier.Reset()
	e.silenceNotifier.ResetPendingRecovery()

	// Stop silence dump manager
	if e.silenceDumpManager != nil {
		e.silenceDumpManager.Stop()
	}

	e.mu.Lock()
	e.state = types.StateStopped
	e.sourceCmd = nil
	e.sourceCancel = nil
	e.mu.Unlock()

	return errors.Join(errs...)
}

// Restart restarts the encoder.
func (e *Encoder) Restart() error {
	if err := e.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	return e.Start()
}

// StartStream initiates a streaming process.
func (e *Encoder) StartStream(streamID string) error {
	var stopChan chan struct{}

	e.mu.RLock()
	if e.state != types.StateRunning {
		e.mu.RUnlock()
		return ErrNotRunning
	}
	stopChan = e.stopChan
	e.mu.RUnlock()

	stream := e.config.Stream(streamID)
	if stream == nil {
		return ErrStreamNotFound
	}
	if !stream.IsEnabled() {
		return ErrStreamDisabled
	}

	// Start preserves existing retry state automatically
	if err := e.streamManager.Start(stream); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	go e.streamManager.MonitorAndRetry(streamID, e, stopChan)

	return nil
}

// StopStream terminates the stream with the given ID.
func (e *Encoder) StopStream(streamID string) error {
	return e.streamManager.Stop(streamID)
}

// TriggerTestEmail sends a test email.
func (e *Encoder) TriggerTestEmail() error {
	cfg := e.config.Snapshot()
	return notify.SendTestEmail(notify.BuildGraphConfig(cfg), cfg.StationName)
}

// GraphSecretExpiry returns the current Graph API client secret expiry info.
func (e *Encoder) GraphSecretExpiry() types.SecretExpiryInfo {
	if e.secretExpiryChecker == nil {
		return types.SecretExpiryInfo{}
	}
	return e.secretExpiryChecker.GetInfo()
}

// InvalidateGraphSecretExpiryCache clears the cached secret expiry info.
// Note: The Graph client in SilenceNotifier automatically detects config changes
// and recreates itself when needed (same pattern as S3 client in recorders).
func (e *Encoder) InvalidateGraphSecretExpiryCache() {
	if e.secretExpiryChecker != nil {
		graphCfg := e.config.GraphConfig()
		e.secretExpiryChecker.UpdateConfig(&graphCfg)
	}
}

// UpdateSilenceConfig updates the silence detection settings.
func (e *Encoder) UpdateSilenceConfig() {
	if e.silenceDetect != nil {
		e.silenceDetect.Reset()
	}
}

// UpdateSilenceDumpConfig updates the silence dump capture settings.
func (e *Encoder) UpdateSilenceDumpConfig() {
	snap := e.config.Snapshot()
	if e.silenceDumpManager != nil {
		e.silenceDumpManager.SetEnabled(snap.SilenceDumpEnabled)
		e.silenceDumpManager.SetRetentionDays(snap.SilenceDumpRetentionDays)
	}
}

// TriggerTestWebhook sends a test webhook.
func (e *Encoder) TriggerTestWebhook() error {
	cfg := e.config.Snapshot()
	return notify.SendTestWebhook(cfg.WebhookURL, cfg.StationName)
}

// TriggerTestZabbix sends a test notification to the configured Zabbix server.
func (e *Encoder) TriggerTestZabbix() error {
	cfg := e.config.Snapshot()
	return notify.SendTestZabbix(cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixKey)
}

// runSourceLoop runs the audio capture process.
func (e *Encoder) runSourceLoop() {
	for {
		e.mu.Lock()
		if e.state == types.StateStopping || e.state == types.StateStopped {
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()

		startTime := time.Now()
		stderrOutput, err := e.runSource()
		runDuration := time.Since(startTime)

		e.mu.Lock()
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
				e.state = types.StateStopped
				e.lastError = fmt.Sprintf("Stopped after %d failed attempts: %s", types.MaxRetries, errMsg)
				e.mu.Unlock()
				if err := e.streamManager.StopAll(); err != nil {
					slog.Error("failed to stop streams during source failure", "error", err)
				}
				return
			}
		} else {
			e.retryCount = 0
			e.backoff.Reset()
		}

		if e.state == types.StateStopping || e.state == types.StateStopped {
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

// runSource executes the audio capture process.
func (e *Encoder) runSource() (string, error) {
	audioInput := e.config.Snapshot().AudioInput
	cmdName, args, err := audio.BuildCaptureCommand(audioInput, e.ffmpegPath)
	if err != nil {
		return "", err
	}

	slog.Info("starting audio capture", "command", cmdName, "input", audioInput)

	ctx, cancel := context.WithCancel(context.Background())
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

	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.sourceCmd = cmd
		e.sourceCancel = cancel
		e.sourceStdout = stdoutPipe
		e.state = types.StateRunning
		e.startTime = time.Now()
		e.lastError = ""
		e.audioLevels = audio.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}
	}()

	if err := cmd.Start(); err != nil {
		return "", err
	}

	// Start distributor and streams after brief delay
	go func() {
		time.Sleep(types.StreamRestartDelay)
		e.startEnabledStreams()
	}()

	err = cmd.Wait()

	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.sourceCmd = nil
		e.sourceCancel = nil
		e.sourceStdout = nil
	}()

	return util.ExtractLastError(stderrBuf.String()), err
}

// startEnabledStreams starts all enabled streaming processes.
func (e *Encoder) startEnabledStreams() {
	// Start silence dump manager (cleanup scheduler)
	if e.silenceDumpManager != nil {
		e.silenceDumpManager.Start()
	}

	go e.runDistributor()

	for _, stream := range e.config.ConfiguredStreams() {
		if !stream.IsEnabled() {
			slog.Info("skipping disabled stream", "stream_id", stream.ID)
			continue
		}
		if err := e.StartStream(stream.ID); err != nil {
			slog.Error("failed to start stream", "stream_id", stream.ID, "error", err)
		}
	}

	// Start recording manager (starts auto-start recorders)
	if err := e.recordingManager.Start(); err != nil {
		slog.Error("failed to start recording manager", "error", err)
	}
}

// runDistributor delivers audio from the source to all streaming processes.
func (e *Encoder) runDistributor() {
	buf := make([]byte, 19200) // ~100ms of audio at 48kHz stereo

	distributor := NewDistributor(
		e.silenceDetect,
		e.silenceNotifier,
		e.silenceDumpManager,
		e.peakHolder,
		e.config,
		e.updateAudioLevels,
	)

	for {
		e.mu.RLock()
		state := e.state
		reader := e.sourceStdout
		stopChan := e.stopChan
		e.mu.RUnlock()

		if state != types.StateRunning || reader == nil {
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

		// Feed audio to silence dump manager
		if e.silenceDumpManager != nil {
			e.silenceDumpManager.WriteAudio(buf[:n])
		}

		distributor.ProcessSamples(buf, n)

		for _, stream := range e.config.ConfiguredStreams() {
			// WriteAudio logs errors internally and marks stream as stopped
			_ = e.streamManager.WriteAudio(stream.ID, buf[:n]) //nolint:errcheck // Errors logged internally by WriteAudio
		}

		// Send audio to recording manager
		_ = e.recordingManager.WriteAudio(buf[:n]) //nolint:errcheck // Errors logged internally by recording manager
	}
}

// updateAudioLevels updates the current audio level readings.
func (e *Encoder) updateAudioLevels(levels *audio.AudioLevels) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.audioLevels = *levels
	e.lastKnownLevels = *levels // Update cache for TryRLock fallback
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
	if err := e.config.AddRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.AddRecorder(cfg)
}

// RemoveRecorder deletes a recorder.
func (e *Encoder) RemoveRecorder(id string) error {
	if err := e.recordingManager.RemoveRecorder(id); err != nil {
		slog.Warn("error removing recorder from manager", "id", id, "error", err)
	}
	return e.config.RemoveRecorder(id)
}

// UpdateRecorder modifies a recorder configuration.
func (e *Encoder) UpdateRecorder(cfg *types.Recorder) error {
	if err := e.config.UpdateRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.UpdateRecorder(cfg)
}

// StartRecorder initiates a recorder.
func (e *Encoder) StartRecorder(id string) error {
	return e.recordingManager.StartRecorder(id)
}

// StopRecorder terminates a recorder.
func (e *Encoder) StopRecorder(id string) error {
	return e.recordingManager.StopRecorder(id)
}
