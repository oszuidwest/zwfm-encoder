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
	cmd           *exec.Cmd
	ctx           context.Context
	cancelCause   context.CancelCauseFunc
	stdin         io.WriteCloser
	stdinMu       sync.Mutex // Protects stdin I/O (Write + Close)
	state         OutputState
	lastError     string
	startTime     time.Time
	retryCount    int
	backoff       *util.Backoff
	startingTimer *time.Timer // Timer for starting timeout (30s)
}

// NewManager creates a new output manager with the specified FFmpeg binary path.
func NewManager(ffmpegPath string) *Manager {
	return &Manager{
		ffmpegPath: ffmpegPath,
		processes:  make(map[string]*Process),
	}
}

// Start starts an output FFmpeg process asynchronously.
// Returns immediately after setting state to "starting".
// The actual FFmpeg startup happens in a background goroutine.
func (m *Manager) Start(output *types.Output) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, exists := m.processes[output.ID]
	if exists {
		switch existing.state {
		case StateStarting, StateConnected:
			return nil // Already running or starting
		case StateGivenUp:
			// Reset given_up state on new start attempt
			existing.state = StateStopped
			existing.retryCount = 0
			if existing.backoff != nil {
				existing.backoff.Reset()
			}
			existing.lastError = ""
		}
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

	// Create process entry with starting state
	proc := &Process{
		state:      StateStarting,
		lastError:  "",
		startTime:  time.Now(),
		retryCount: retryCount,
		backoff:    backoff,
	}

	m.processes[output.ID] = proc

	// Start FFmpeg in background goroutine
	go m.startAsync(output.ID, output)

	return nil
}

// startAsync performs the actual FFmpeg startup in a background goroutine.
func (m *Manager) startAsync(outputID string, output *types.Output) {
	args := BuildFFmpegArgs(output)

	slog.Info("starting output", "output_id", outputID, "host", output.Host, "port", output.Port)

	ctx, cancelCause := context.WithCancelCause(context.Background())
	cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancelCause(errors.New("failed to create stdin pipe"))
		m.transitionToError(outputID, fmt.Sprintf("stdin pipe: %v", err))
		return
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Update process with cmd and stdin
	m.mu.Lock()
	proc, exists := m.processes[outputID]
	if !exists || proc.state != StateStarting {
		m.mu.Unlock()
		cancelCause(errors.New("output state changed during startup"))
		_ = stdinPipe.Close()
		return
	}

	proc.cmd = cmd
	proc.ctx = ctx
	proc.cancelCause = cancelCause
	proc.stdin = stdinPipe
	m.mu.Unlock()

	if err := cmd.Start(); err != nil {
		cancelCause(fmt.Errorf("failed to start: %w", err))
		_ = stdinPipe.Close()
		m.transitionToError(outputID, fmt.Sprintf("start ffmpeg: %v", err))
		return
	}

	// Start the starting timeout timer (30 seconds)
	m.mu.Lock()
	if proc, exists := m.processes[outputID]; exists && proc.state == StateStarting {
		proc.startingTimer = time.AfterFunc(StartingTimeout, func() {
			m.handleStartingTimeout(outputID)
		})
	}
	m.mu.Unlock()
}

// transitionToError moves an output to error state.
func (m *Manager) transitionToError(outputID, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, exists := m.processes[outputID]
	if !exists {
		return
	}

	proc.state = StateError
	proc.lastError = errMsg

	// Stop the starting timer if running
	if proc.startingTimer != nil {
		proc.startingTimer.Stop()
		proc.startingTimer = nil
	}

	slog.Error("output error", "output_id", outputID, "error", errMsg)
}

// handleStartingTimeout is called when starting state exceeds 30 seconds.
func (m *Manager) handleStartingTimeout(outputID string) {
	m.mu.Lock()
	proc, exists := m.processes[outputID]
	if !exists || proc.state != StateStarting {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	slog.Warn("output starting timeout exceeded", "output_id", outputID, "timeout", StartingTimeout)
	m.transitionToError(outputID, "connection timeout after 30s")
}

// transitionToConnected moves an output from starting to connected state.
func (m *Manager) transitionToConnected(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, exists := m.processes[outputID]
	if !exists || proc.state != StateStarting {
		return
	}

	proc.state = StateConnected

	// Stop and clear the starting timer
	if proc.startingTimer != nil {
		proc.startingTimer.Stop()
		proc.startingTimer = nil
	}

	// Reset retry count on successful stable connection
	proc.retryCount = 0
	if proc.backoff != nil {
		proc.backoff.Reset()
	}
	proc.lastError = ""

	slog.Info("output connected", "output_id", outputID)
}

// checkAndTransitionToConnected checks if output should transition to connected.
func (m *Manager) checkAndTransitionToConnected(outputID string) {
	m.mu.RLock()
	proc, exists := m.processes[outputID]
	if !exists || proc.state != StateStarting {
		m.mu.RUnlock()
		return
	}
	startTime := proc.startTime
	m.mu.RUnlock()

	// Check if stable threshold reached (10 seconds)
	if time.Since(startTime) >= types.StableThreshold {
		m.transitionToConnected(outputID)
	}
}

// Stop stops an output process.
func (m *Manager) Stop(outputID string) error {
	m.mu.Lock()
	proc, exists := m.processes[outputID]
	if !exists {
		m.mu.Unlock()
		return nil
	}

	// Already stopped or stopping
	if proc.state == StateStopped || proc.state == StateStopping {
		delete(m.processes, outputID)
		m.mu.Unlock()
		return nil
	}

	// Stop the starting timer if running
	if proc.startingTimer != nil {
		proc.startingTimer.Stop()
		proc.startingTimer = nil
	}

	proc.state = StateStopping
	process := proc.cmd
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
	if process != nil && process.Process != nil {
		_ = util.GracefulSignal(process.Process)
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
	// Only write to outputs in starting or connected state
	if !exists || (proc.state != StateStarting && proc.state != StateConnected) {
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
		// Re-check proc still exists and is still in an active state
		if proc, exists := m.processes[outputID]; exists && (proc.state == StateStarting || proc.state == StateConnected) {
			slog.Warn("output write failed, transitioning to error", "output_id", outputID, "error", err)
			proc.state = StateError
			proc.lastError = err.Error()
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
			State:      string(proc.state),
			LastError:  proc.lastError,
			RetryCount: proc.retryCount,
			MaxRetries: maxRetries,
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

// MarkStopped marks an output as stopped (used after process exits).
func (m *Manager) MarkStopped(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, exists := m.processes[outputID]; exists {
		// Only transition to error if not already in a terminal state
		if proc.state == StateStarting || proc.state == StateConnected {
			proc.state = StateError
		}
	}
}

// ClearErrorState clears error or given_up state for an output.
// Called when output config is updated.
func (m *Manager) ClearErrorState(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, exists := m.processes[outputID]
	if !exists {
		return
	}

	if proc.state == StateError || proc.state == StateGivenUp {
		proc.state = StateStopped
		proc.lastError = ""
		proc.retryCount = 0
		if proc.backoff != nil {
			proc.backoff.Reset()
		}
		slog.Info("output error state cleared", "output_id", outputID)
	}
}

// ResetAllGivenUp resets given_up state for all outputs.
// Called when encoder restarts.
func (m *Manager) ResetAllGivenUp() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, proc := range m.processes {
		if proc.state == StateGivenUp {
			proc.state = StateStopped
			proc.retryCount = 0
			if proc.backoff != nil {
				proc.backoff.Reset()
			}
			slog.Info("output given_up state reset", "output_id", id)
		}
	}
}

// transitionToGivenUp moves an output to given_up state.
func (m *Manager) transitionToGivenUp(outputID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, exists := m.processes[outputID]
	if !exists {
		return
	}

	proc.state = StateGivenUp
	slog.Info("output given up after max retries", "output_id", outputID, "retry_count", proc.retryCount)
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

	m.mu.RLock()
	proc, exists := m.processes[outputID]
	if !exists {
		m.mu.RUnlock()
		return false, "process removed"
	}
	retryCount := proc.retryCount
	state := proc.state
	m.mu.RUnlock()

	// Don't retry if already given up
	if state == StateGivenUp {
		return false, "max retries exceeded"
	}

	maxRetries := out.MaxRetriesOrDefault()
	if retryCount > maxRetries {
		m.transitionToGivenUp(outputID)
		return false, "max retries exceeded"
	}
	return true, ""
}

// MonitorAndRetry monitors an output process and handles automatic retry on failure.
func (m *Manager) MonitorAndRetry(outputID string, ctx OutputContext, stopChan <-chan struct{}) {
	// Ticker for periodic state checks (starting → connected transition)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

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

		// Run cmd.Wait() in a goroutine so we can also check state transitions
		waitDone := make(chan error, 1)
		startTime := time.Now()
		go func() {
			waitDone <- cmd.Wait()
		}()

		// Wait for process exit while periodically checking state
	waitLoop:
		for {
			select {
			case <-stopChan:
				m.Remove(outputID)
				return
			case <-ticker.C:
				// Check for starting → connected transition
				m.checkAndTransitionToConnected(outputID)
			case err := <-waitDone:
				runDuration := time.Since(startTime)
				m.MarkStopped(outputID)
				m.handleProcessExit(outputID, cmd, procCtx, backoff, err, runDuration)
				break waitLoop
			}
		}

		shouldRetry, reason := m.shouldContinueRetry(outputID, ctx)
		if !shouldRetry {
			if reason != "" {
				slog.Info("output monitoring stopped", "output_id", outputID, "reason", reason)
			}
			// Don't remove if given up - keep for status display
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
