// Package encoder provides the audio capture and encoding engine.
// It manages real-time PCM audio distribution to multiple FFmpeg output
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
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/output"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// LevelUpdateSamples is the number of samples before updating audio levels.
const LevelUpdateSamples = 12000

// Sentinel errors for encoder operations.
var (
	ErrNoAudioInput   = errors.New("no audio input configured")
	ErrAlreadyRunning = errors.New("encoder already running")
	ErrNotRunning     = errors.New("encoder not running")
	ErrOutputDisabled = errors.New("output is disabled")
	ErrOutputNotFound = errors.New("output not found")
)

// Encoder manages audio capture and distribution to multiple streaming outputs.
type Encoder struct {
	config           *config.Config
	ffmpegPath       string
	outputManager    *output.Manager
	recordingManager *recording.Manager
	sourceCmd        *exec.Cmd
	sourceCancel     context.CancelFunc
	sourceStdout     io.ReadCloser
	state            types.EncoderState
	stopChan         chan struct{}
	mu               sync.RWMutex
	lastError        string
	startTime        time.Time
	retryCount       int
	backoff          *util.Backoff
	audioLevels      types.AudioLevels
	lastKnownLevels  types.AudioLevels // Cache for TryRLock fallback
	silenceDetect    *audio.SilenceDetector
	silenceNotifier  *notify.SilenceNotifier
	peakHolder       *audio.PeakHolder
}

// New creates a new Encoder with the given configuration and FFmpeg binary path.
func New(cfg *config.Config, ffmpegPath string) *Encoder {
	return &Encoder{
		config:          cfg,
		ffmpegPath:      ffmpegPath,
		outputManager:   output.NewManager(ffmpegPath),
		state:           types.StateStopped,
		backoff:         util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay),
		silenceDetect:   audio.NewSilenceDetector(),
		silenceNotifier: notify.NewSilenceNotifier(cfg),
		peakHolder:      audio.NewPeakHolder(),
	}
}

// InitRecording initializes the recording manager.
// This should be called before Start(). Always creates the manager
// so recorders can be added at runtime.
func (e *Encoder) InitRecording() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap := e.config.Snapshot()
	apiKey := e.config.GetRecordingAPIKey()

	mgr, err := recording.NewManager(e.ffmpegPath, apiKey, "", snap.RecordingMaxDurationMinutes)
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
func (e *Encoder) AllRecorderStatuses() map[string]types.RecorderStatus {
	return e.recordingManager.AllStatuses()
}

// State returns the current encoder state.
func (e *Encoder) State() types.EncoderState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Output returns the output configuration for the given ID.
func (e *Encoder) Output(outputID string) *types.Output {
	return e.config.Output(outputID)
}

// IsRunning reports whether the encoder is in running state.
func (e *Encoder) IsRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == types.StateRunning
}

// AudioLevels returns the current audio levels.
func (e *Encoder) AudioLevels() types.AudioLevels {
	if !e.mu.TryRLock() {
		return e.lastKnownLevels
	}
	defer e.mu.RUnlock()

	if e.state != types.StateRunning {
		return types.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}
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

// AllOutputStatuses returns status for all configured outputs.
// This includes disabled outputs (with Disabled: true) and outputs without
// active processes, ensuring the frontend always has complete status data.
func (e *Encoder) AllOutputStatuses(outputs []types.Output) map[string]types.OutputStatus {
	// Get statuses for outputs with active processes
	processStatuses := e.outputManager.AllStatuses(func(id string) int {
		if o := e.config.Output(id); o != nil {
			return o.MaxRetriesOrDefault()
		}
		return types.DefaultMaxRetries
	})

	// Build complete status map for all configured outputs
	result := make(map[string]types.OutputStatus, len(outputs))
	for _, out := range outputs {
		if status, exists := processStatuses[out.ID]; exists {
			// Output has active process - use its status
			// Also reflect disabled state if output was disabled while running
			if !out.IsEnabled() {
				status.Disabled = true
			}
			result[out.ID] = status
		} else if !out.IsEnabled() {
			// Output is disabled - mark explicitly
			result[out.ID] = types.OutputStatus{
				Disabled:   true,
				MaxRetries: out.MaxRetriesOrDefault(),
			}
		} else {
			// Output is enabled but has no process (encoder not running)
			result[out.ID] = types.OutputStatus{
				MaxRetries: out.MaxRetriesOrDefault(),
			}
		}
	}
	return result
}

// Start begins audio capture and all output processes.
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

// Stop stops all processes with graceful shutdown.
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

	// Stop all outputs first
	if err := e.outputManager.StopAll(); err != nil {
		errs = append(errs, fmt.Errorf("stop outputs: %w", err))
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

	stopped := e.pollUntil(func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.sourceCmd == nil
	})

	select {
	case <-stopped:
		slog.Info("source capture stopped gracefully")
	case <-time.After(types.ShutdownTimeout):
		slog.Warn("source capture did not stop in time, forcing kill")
		if sourceCancel != nil {
			sourceCancel()
		}
		errs = append(errs, fmt.Errorf("source shutdown timeout"))
	}

	e.mu.Lock()
	e.state = types.StateStopped
	e.sourceCmd = nil
	e.sourceCancel = nil
	e.mu.Unlock()

	return errors.Join(errs...)
}

// Restart stops and starts the encoder.
func (e *Encoder) Restart() error {
	if err := e.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	return e.Start()
}

// StartOutput starts an individual output FFmpeg process.
func (e *Encoder) StartOutput(outputID string) error {
	var stopChan chan struct{}

	e.mu.RLock()
	if e.state != types.StateRunning {
		e.mu.RUnlock()
		return ErrNotRunning
	}
	stopChan = e.stopChan
	e.mu.RUnlock()

	out := e.config.Output(outputID)
	if out == nil {
		return ErrOutputNotFound
	}
	if !out.IsEnabled() {
		return ErrOutputDisabled
	}

	// Start preserves existing retry state automatically
	if err := e.outputManager.Start(out); err != nil {
		return fmt.Errorf("failed to start output: %w", err)
	}

	go e.outputManager.MonitorAndRetry(outputID, e, stopChan)

	return nil
}

// StopOutput stops an output by ID.
func (e *Encoder) StopOutput(outputID string) error {
	return e.outputManager.Stop(outputID)
}

// TriggerTestEmail sends a test email to verify configuration.
func (e *Encoder) TriggerTestEmail() error {
	cfg := e.config.Snapshot()
	return notify.SendTestEmail(notify.BuildEmailConfig(cfg), cfg.StationName)
}

// TriggerTestWebhook sends a test webhook to verify configuration.
func (e *Encoder) TriggerTestWebhook() error {
	cfg := e.config.Snapshot()
	return notify.SendTestWebhook(cfg.WebhookURL, cfg.StationName)
}

// TriggerTestLog writes a test entry to verify log file configuration.
func (e *Encoder) TriggerTestLog() error {
	return notify.WriteTestLog(e.config.Snapshot().LogPath)
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
				if err := e.outputManager.StopAll(); err != nil {
					slog.Error("failed to stop outputs during source failure", "error", err)
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
		e.audioLevels = types.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}
	}()

	if err := cmd.Start(); err != nil {
		return "", err
	}

	// Start distributor and outputs after brief delay
	go func() {
		time.Sleep(types.OutputRestartDelay)
		e.startEnabledOutputs()
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

// startEnabledOutputs starts the audio distributor and all enabled output processes.
func (e *Encoder) startEnabledOutputs() {
	go e.runDistributor()

	for _, out := range e.config.ConfiguredOutputs() {
		if !out.IsEnabled() {
			slog.Info("skipping disabled output", "output_id", out.ID)
			continue
		}
		if err := e.StartOutput(out.ID); err != nil {
			slog.Error("failed to start output", "output_id", out.ID, "error", err)
		}
	}

	// Start recording manager (starts auto-start recorders)
	if err := e.recordingManager.Start(); err != nil {
		slog.Error("failed to start recording manager", "error", err)
	}
}

// runDistributor delivers audio from the source to all output processes.
func (e *Encoder) runDistributor() {
	buf := make([]byte, 19200) // ~100ms of audio at 48kHz stereo

	// Snapshot silence config once at startup (avoids mutex contention in hot path)
	cfg := e.config.Snapshot()
	silenceCfg := audio.SilenceConfig{
		Threshold:  cfg.SilenceThreshold,
		DurationMs: cfg.SilenceDurationMs,
		RecoveryMs: cfg.SilenceRecoveryMs,
	}

	distributor := NewDistributor(
		e.silenceDetect,
		e.silenceNotifier,
		e.peakHolder,
		silenceCfg,
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

		distributor.ProcessSamples(buf, n)

		for _, out := range e.config.ConfiguredOutputs() {
			// WriteAudio logs errors internally and marks output as stopped
			_ = e.outputManager.WriteAudio(out.ID, buf[:n]) //nolint:errcheck // Errors logged internally by WriteAudio
		}

		// Send audio to recording manager
		_ = e.recordingManager.WriteAudio(buf[:n]) //nolint:errcheck // Errors logged internally by recording manager
	}
}

// updateAudioLevels updates audio levels from calculated metrics.
func (e *Encoder) updateAudioLevels(m *types.AudioMetrics) {
	levels := types.AudioLevels{
		Left:              m.RMSLeft,
		Right:             m.RMSRight,
		PeakLeft:          m.PeakLeft,
		PeakRight:         m.PeakRight,
		Silence:           m.Silence,
		SilenceDurationMs: m.SilenceDurationMs,
		SilenceLevel:      m.SilenceLevel,
		ClipLeft:          m.ClipLeft,
		ClipRight:         m.ClipRight,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.audioLevels = levels
	e.lastKnownLevels = levels // Update cache for TryRLock fallback
}

// pollUntil signals when the given condition becomes true.
func (e *Encoder) pollUntil(condition func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for !condition() {
			time.Sleep(types.PollInterval)
		}
		close(done)
	}()
	return done
}

// AddRecorder adds a new recorder and saves to config.
func (e *Encoder) AddRecorder(cfg *types.Recorder) error {
	if err := e.config.AddRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.AddRecorder(cfg)
}

// RemoveRecorder removes a recorder and saves to config.
func (e *Encoder) RemoveRecorder(id string) error {
	if err := e.recordingManager.RemoveRecorder(id); err != nil {
		slog.Warn("error removing recorder from manager", "id", id, "error", err)
	}
	return e.config.RemoveRecorder(id)
}

// UpdateRecorder updates a recorder configuration.
func (e *Encoder) UpdateRecorder(cfg *types.Recorder) error {
	if err := e.config.UpdateRecorder(cfg); err != nil {
		return err
	}
	return e.recordingManager.UpdateRecorder(cfg)
}

// StartRecorder starts a specific recorder.
func (e *Encoder) StartRecorder(id string) error {
	return e.recordingManager.StartRecorder(id)
}

// StopRecorder stops a specific recorder.
func (e *Encoder) StopRecorder(id string) error {
	return e.recordingManager.StopRecorder(id)
}
