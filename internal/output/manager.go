package output

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// errStoppedByUser indicates the output was intentionally stopped.
var errStoppedByUser = errors.New("stopped by user")

// OutputContext provides encoder state for monitoring and retry decisions.
type OutputContext interface {
	// Output returns the output configuration, or nil if removed.
	Output(outputID string) *types.Output
	// IsRunning reports whether the encoder is in running state.
	IsRunning() bool
}

// Manager manages multiple output FFmpeg processes.
//
// Concurrency: Uses mu (RWMutex) to protect the processes map.
// Each Process has its own stdinMu to protect I/O operations.
type Manager struct {
	ffmpegPath string
	processes  map[string]*Process
	mu         sync.RWMutex // Protects processes map
}

// Process tracks an individual output FFmpeg process.
//
// Concurrency: stdinMu protects stdin from concurrent access.
// This prevents a race where WriteAudio() writes while Stop() closes stdin.
type Process struct {
	cmd         *exec.Cmd
	ctx         context.Context
	cancelCause context.CancelCauseFunc
	stdin       io.WriteCloser
	stdinMu     sync.Mutex // Protects stdin I/O (Write + Close)
	running     bool
	lastError   string
	startTime   time.Time
	retryCount  int
	backoff     *util.Backoff
}

// NewManager creates a new output manager with the specified FFmpeg binary path.
func NewManager(ffmpegPath string) *Manager {
	return &Manager{
		ffmpegPath: ffmpegPath,
		processes:  make(map[string]*Process),
	}
}

// Start starts an output FFmpeg process.
func (m *Manager) Start(output *types.Output) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, exists := m.processes[output.ID]
	if exists && existing.running {
		return nil // Already running
	}

	// Preserve retry state from existing entry, or create fresh
	var retryCount int
	var backoff *util.Backoff
	if exists && existing.backoff != nil {
		retryCount = existing.retryCount
		backoff = existing.backoff
	} else {
		retryCount = 0
		backoff = util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay)
	}

	args := BuildFFmpegArgs(output)

	slog.Info("starting output", "output_id", output.ID, "host", output.Host, "port", output.Port)

	ctx, cancelCause := context.WithCancelCause(context.Background())
	cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancelCause(errors.New("failed to create stdin pipe"))
		return fmt.Errorf("stdin pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	proc := &Process{
		cmd:         cmd,
		ctx:         ctx,
		cancelCause: cancelCause,
		stdin:       stdinPipe,
		running:     true,
		startTime:   time.Now(),
		retryCount:  retryCount,
		backoff:     backoff,
	}

	m.processes[output.ID] = proc

	if err := cmd.Start(); err != nil {
		cancelCause(fmt.Errorf("failed to start: %w", err))
		if closeErr := stdinPipe.Close(); closeErr != nil {
			slog.Warn("failed to close stdin pipe", "error", closeErr)
		}
		delete(m.processes, output.ID)
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	return nil
}

// Stop stops an output process.
func (m *Manager) Stop(outputID string) error {
	m.mu.Lock()
	proc, exists := m.processes[outputID]
	if !exists {
		m.mu.Unlock()
		return nil
	}

	if !proc.running {
		delete(m.processes, outputID)
		m.mu.Unlock()
		return nil
	}

	proc.running = false
	process := proc.cmd.Process
	cancelCause := proc.cancelCause
	m.mu.Unlock()

	slog.Info("stopping output", "output_id", outputID)

	// Close stdin under stdinMu to prevent race with WriteAudio
	proc.stdinMu.Lock()
	stdin := proc.stdin
	proc.stdin = nil
	proc.stdinMu.Unlock()

	if stdin != nil {
		if err := stdin.Close(); err != nil {
			slog.Warn("failed to close stdin", "error", err)
		}
	}

	// Mark as intentionally stopped before signaling
	if cancelCause != nil {
		cancelCause(errStoppedByUser)
	}
	if process != nil {
		_ = util.GracefulSignal(process)
	}

	// Wait briefly for graceful exit
	time.Sleep(100 * time.Millisecond)

	m.mu.Lock()
	delete(m.processes, outputID)
	m.mu.Unlock()

	return nil
}

// StopAll stops all outputs and returns any errors that occurred.
func (m *Manager) StopAll() error {
	m.mu.RLock()
	ids := slices.Collect(maps.Keys(m.processes))
	m.mu.RUnlock()

	var errs []error
	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			slog.Error("failed to stop output", "output_id", id, "error", err)
			errs = append(errs, err)
		}
	}

	m.mu.Lock()
	clear(m.processes)
	m.mu.Unlock()

	return errors.Join(errs...)
}

// WriteAudio writes audio data to a specific output.
// Uses proc.stdinMu to prevent race with concurrent stdin.Close() in Stop.
func (m *Manager) WriteAudio(outputID string, data []byte) error {
	// Get process under read lock
	m.mu.RLock()
	proc, exists := m.processes[outputID]
	if !exists || !proc.running {
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()

	// Lock stdinMu to prevent race with Stop closing stdin
	proc.stdinMu.Lock()
	stdin := proc.stdin
	if stdin == nil {
		proc.stdinMu.Unlock()
		return nil
	}
	_, err := stdin.Write(data)
	proc.stdinMu.Unlock()

	if err != nil {
		// Update state under write lock on error
		m.mu.Lock()
		// Re-check proc still exists and hasn't been modified
		if proc, exists := m.processes[outputID]; exists && proc.running {
			slog.Warn("output write failed, marking as stopped", "output_id", outputID, "error", err)
			proc.running = false
			proc.stdinMu.Lock()
			if proc.stdin != nil {
				if closeErr := proc.stdin.Close(); closeErr != nil {
					slog.Warn("failed to close stdin", "error", closeErr)
				}
				proc.stdin = nil
			}
			proc.stdinMu.Unlock()
		}
		m.mu.Unlock()
		return fmt.Errorf("write audio: %w", err)
	}
	return nil
}

// AllStatuses returns status for all tracked outputs.
func (m *Manager) AllStatuses(getMaxRetries func(string) int) map[string]types.OutputStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]types.OutputStatus)
	for id, proc := range m.processes {
		maxRetries := getMaxRetries(id)
		statuses[id] = types.OutputStatus{
			Running:    proc.running,
			Stable:     proc.running && time.Since(proc.startTime) >= types.StableThreshold,
			LastError:  proc.lastError,
			RetryCount: proc.retryCount,
			MaxRetries: maxRetries,
			GivenUp:    proc.retryCount > maxRetries,
		}
	}
	return statuses
}

// Process returns process info for monitoring.
func (m *Manager) Process(outputID string) (cmd *exec.Cmd, ctx context.Context, retryCount int, backoff *util.Backoff, exists bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	proc, exists := m.processes[outputID]
	if !exists {
		return nil, nil, 0, nil, false
	}
	return proc.cmd, proc.ctx, proc.retryCount, proc.backoff, true
}

// SetError sets the last error for an output.
func (m *Manager) SetError(outputID, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, exists := m.processes[outputID]; exists {
		proc.lastError = errMsg
	}
}

// IncrementRetry increments the retry count for an output.
func (m *Manager) IncrementRetry(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, exists := m.processes[outputID]; exists {
		proc.retryCount++
	}
}

// ResetRetry resets the retry count and backoff for an output.
func (m *Manager) ResetRetry(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, exists := m.processes[outputID]; exists {
		proc.retryCount = 0
		if proc.backoff != nil {
			proc.backoff.Reset()
		}
	}
}

// MarkStopped marks an output as not running.
func (m *Manager) MarkStopped(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, exists := m.processes[outputID]; exists {
		proc.running = false
	}
}

// Remove removes an output from tracking.
func (m *Manager) Remove(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.processes, outputID)
}

// RetryCount returns the current retry count for an output.
func (m *Manager) RetryCount(outputID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if proc, exists := m.processes[outputID]; exists {
		return proc.retryCount
	}
	return 0
}

// handleProcessExit processes the result of a terminated FFmpeg process.
// It logs errors, updates retry state, and advances the backoff if needed.
func (m *Manager) handleProcessExit(outputID string, cmd *exec.Cmd, procCtx context.Context, backoff *util.Backoff, err error, runDuration time.Duration) {
	// Check if this was an intentional stop - don't treat as error
	cause := context.Cause(procCtx)
	if errors.Is(cause, errStoppedByUser) {
		return
	}

	if err != nil {
		var errMsg string
		if stderr, ok := cmd.Stderr.(*bytes.Buffer); ok && stderr != nil {
			errMsg = util.ExtractLastError(stderr.String())
		}
		if errMsg == "" {
			errMsg = err.Error()
		}
		if cause != nil {
			slog.Error("output error", "output_id", outputID, "error", errMsg, "cause", cause)
		} else {
			slog.Error("output error", "output_id", outputID, "error", errMsg)
		}
		m.SetError(outputID, errMsg)

		if runDuration >= types.SuccessThreshold {
			m.ResetRetry(outputID)
		} else {
			m.IncrementRetry(outputID)
			backoff.Next()
		}
	} else {
		m.ResetRetry(outputID)
	}
}

// shouldContinueRetry checks if the output should continue retrying.
// Returns false with a reason if retry should stop.
func (m *Manager) shouldContinueRetry(outputID string, ctx OutputContext) (shouldRetry bool, reason string) {
	if !ctx.IsRunning() {
		return false, "encoder stopped"
	}
	out := ctx.Output(outputID)
	if out == nil {
		return false, "output removed"
	}
	if !out.IsEnabled() {
		return false, "output disabled"
	}
	retryCount := m.RetryCount(outputID)
	maxRetries := out.MaxRetriesOrDefault()
	if retryCount > maxRetries {
		return false, "max retries exceeded"
	}
	return true, ""
}

// MonitorAndRetry monitors an output process and handles automatic retry on failure.
func (m *Manager) MonitorAndRetry(outputID string, ctx OutputContext, stopChan <-chan struct{}) {
	for {
		select {
		case <-stopChan:
			m.Remove(outputID)
			return
		default:
		}

		cmd, procCtx, _, backoff, exists := m.Process(outputID)
		if !exists || cmd == nil || backoff == nil {
			return
		}

		startTime := time.Now()
		err := cmd.Wait()
		runDuration := time.Since(startTime)

		m.MarkStopped(outputID)
		m.handleProcessExit(outputID, cmd, procCtx, backoff, err, runDuration)

		shouldRetry, reason := m.shouldContinueRetry(outputID, ctx)
		if !shouldRetry {
			if reason != "" {
				slog.Info("output monitoring stopped", "output_id", outputID, "reason", reason)
			}
			if reason != "max retries exceeded" {
				m.Remove(outputID)
			}
			return
		}

		retryDelay := backoff.Current()
		retryCount := m.RetryCount(outputID)
		out := ctx.Output(outputID)
		slog.Info("output stopped, waiting before retry",
			"output_id", outputID, "delay", retryDelay, "retry", retryCount, "max_retries", out.MaxRetriesOrDefault())

		select {
		case <-stopChan:
			m.Remove(outputID)
			return
		case <-time.After(retryDelay):
		}

		// Re-check conditions after wait
		shouldRetry, reason = m.shouldContinueRetry(outputID, ctx)
		if !shouldRetry {
			slog.Info("output not restarting", "output_id", outputID, "reason", reason)
			if reason != "max retries exceeded" {
				m.Remove(outputID)
			}
			return
		}

		out = ctx.Output(outputID)
		if err := m.Start(out); err != nil {
			slog.Error("failed to restart output", "output_id", outputID, "error", err)
			m.Remove(outputID)
			return
		}
	}
}
