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
	"syscall"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/output"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// LevelUpdateSamples is the number of samples before updating audio levels (~250ms).
const LevelUpdateSamples = 12000

// Encoder orchestrates audio capture from a platform-specific source (arecord
// on Linux, FFmpeg on macOS) and distributes PCM audio to multiple output
// FFmpeg processes. It handles automatic retry with exponential backoff,
// calculates real-time audio levels, and detects silence conditions.
type Encoder struct {
	config          *config.Config
	outputManager   *output.Manager
	sourceCmd       *exec.Cmd
	sourceCancel    context.CancelFunc
	sourceStdout    io.ReadCloser
	state           types.EncoderState
	stopChan        chan struct{}
	mu              sync.RWMutex
	lastError       string
	startTime       time.Time
	retryCount      int
	backoff         *util.Backoff
	audioLevels     types.AudioLevels
	lastKnownLevels types.AudioLevels // Cache for TryRLock fallback
	silenceDetect   *audio.SilenceDetector
	silenceNotifier *notify.SilenceNotifier
	peakHolder      *audio.PeakHolder
}

// New creates a new Encoder with the given configuration.
func New(cfg *config.Config) *Encoder {
	return &Encoder{
		config:          cfg,
		outputManager:   output.NewManager(),
		state:           types.StateStopped,
		backoff:         util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay),
		silenceDetect:   audio.NewSilenceDetector(),
		silenceNotifier: notify.NewSilenceNotifier(cfg),
		peakHolder:      audio.NewPeakHolder(),
	}
}

// State returns the current encoder state.
func (e *Encoder) State() types.EncoderState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Output returns the output configuration for the given ID.
// Implements output.OutputContext.
func (e *Encoder) Output(outputID string) *types.Output {
	return e.config.Output(outputID)
}

// IsRunning returns true if the encoder is in running state.
// Implements output.OutputContext.
func (e *Encoder) IsRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == types.StateRunning
}

// AudioLevels returns the current audio levels.
//
// This method uses TryRLock to avoid blocking the 10 fps WebSocket level updates
// during encoder state changes (start/stop) that hold the write lock. Stale cached
// levels are acceptable for VU meter display purposes where visual smoothness
// matters more than sub-100ms accuracy.
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

// AllOutputStatuses returns status for all tracked outputs.
func (e *Encoder) AllOutputStatuses() map[string]types.OutputStatus {
	return e.outputManager.AllStatuses(func(id string) int {
		if o := e.config.Output(id); o != nil {
			return o.MaxRetriesOrDefault()
		}
		return types.DefaultMaxRetries
	})
}

// Start begins audio capture and all output processes.
func (e *Encoder) Start() error {
	e.mu.Lock()

	if e.state == types.StateRunning || e.state == types.StateStarting {
		e.mu.Unlock()
		return fmt.Errorf("encoder already running")
	}

	e.state = types.StateStarting
	e.stopChan = make(chan struct{})
	e.retryCount = 0
	e.backoff.Reset(types.InitialRetryDelay)
	e.silenceDetect.Reset()
	e.silenceNotifier.Reset()
	e.peakHolder.Reset()
	e.mu.Unlock()

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

	// Send SIGINT to source for graceful shutdown
	if sourceProcess != nil && sourceProcess.Process != nil {
		if err := sourceProcess.Process.Signal(syscall.SIGINT); err != nil {
			slog.Warn("failed to send SIGINT to source", "error", err)
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
	time.Sleep(1 * time.Second)
	return e.Start()
}

// StartOutput starts an individual output FFmpeg process.
func (e *Encoder) StartOutput(outputID string) error {
	e.mu.RLock()
	if e.state != types.StateRunning {
		e.mu.RUnlock()
		return fmt.Errorf("encoder not running")
	}
	stopChan := e.stopChan
	e.mu.RUnlock()

	out := e.config.Output(outputID)
	if out == nil {
		return fmt.Errorf("output not found: %s", outputID)
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

// buildEmailConfig constructs an EmailConfig from the current configuration.
func (e *Encoder) buildEmailConfig() *notify.EmailConfig {
	cfg := e.config.Snapshot()
	return &notify.EmailConfig{
		Host:       cfg.EmailSMTPHost,
		Port:       cfg.EmailSMTPPort,
		FromName:   cfg.EmailFromName,
		Username:   cfg.EmailUsername,
		Password:   cfg.EmailPassword,
		Recipients: cfg.EmailRecipients,
	}
}

// TriggerTestEmail sends a test email to verify configuration.
func (e *Encoder) TriggerTestEmail() error {
	return notify.SendTestEmail(e.buildEmailConfig())
}

// TriggerTestWebhook sends a test webhook to verify configuration.
func (e *Encoder) TriggerTestWebhook() error {
	return notify.SendTestWebhook(e.config.Snapshot().WebhookURL)
}

// TriggerTestLog writes a test entry to verify log file configuration.
func (e *Encoder) TriggerTestLog() error {
	return notify.WriteTestLog(e.config.Snapshot().LogPath)
}

// runSourceLoop runs the audio capture process with auto-restart.
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
				e.backoff.Reset(types.InitialRetryDelay)
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
			e.backoff.Reset(types.InitialRetryDelay)
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
	cmdName, args, err := audio.BuildCaptureCommand(audioInput)
	if err != nil {
		return "", err
	}

	slog.Info("starting audio capture", "command", cmdName, "input", audioInput)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, cmdName, args...)

	// Go 1.20+: Declarative graceful shutdown - sends SIGINT first, waits, then SIGKILL
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGINT)
	}
	cmd.WaitDelay = types.ShutdownTimeout

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", err
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	e.mu.Lock()
	e.sourceCmd = cmd
	e.sourceCancel = cancel
	e.sourceStdout = stdoutPipe
	e.state = types.StateRunning
	e.startTime = time.Now()
	e.lastError = ""
	e.audioLevels = types.AudioLevels{Left: -60, Right: -60, PeakLeft: -60, PeakRight: -60}
	e.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return "", err
	}

	// Start distributor and outputs after brief delay
	go func() {
		time.Sleep(500 * time.Millisecond)
		e.startEnabledOutputs()
	}()

	err = cmd.Wait()

	e.mu.Lock()
	e.sourceCmd = nil
	e.sourceCancel = nil
	e.sourceStdout = nil
	e.mu.Unlock()

	return util.ExtractLastError(stderrBuf.String()), err
}

// startEnabledOutputs starts the audio distributor and all output processes.
func (e *Encoder) startEnabledOutputs() {
	go e.runDistributor()

	for _, out := range e.config.ConfiguredOutputs() {
		if err := e.StartOutput(out.ID); err != nil {
			slog.Error("failed to start output", "output_id", out.ID, "error", err)
		}
	}
}

// runDistributor delivers audio from the source to all output processes and calculates audio levels.
func (e *Encoder) runDistributor() {
	buf := make([]byte, 19200) // ~100ms of audio at 48kHz stereo

	// Snapshot silence config once at startup (avoids mutex contention in hot path)
	cfg := e.config.Snapshot()
	silenceCfg := audio.SilenceConfig{
		Threshold: cfg.SilenceThreshold,
		Duration:  cfg.SilenceDuration,
		Recovery:  cfg.SilenceRecovery,
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
	}
}

// updateAudioLevels updates audio levels from calculated metrics.
func (e *Encoder) updateAudioLevels(m *types.AudioMetrics) {
	levels := types.AudioLevels{
		Left:            m.RMSL,
		Right:           m.RMSR,
		PeakLeft:        m.PeakL,
		PeakRight:       m.PeakR,
		Silence:         m.Silence,
		SilenceDuration: m.SilenceDuration,
		SilenceLevel:    m.SilenceLevel,
		ClipLeft:        m.ClipL,
		ClipRight:       m.ClipR,
	}

	e.mu.Lock()
	e.audioLevels = levels
	e.lastKnownLevels = levels // Update cache for TryRLock fallback
	e.mu.Unlock()
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
