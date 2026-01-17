package streaming

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// errStoppedByUser indicates the stream was intentionally stopped.
var errStoppedByUser = errors.New("stopped by user")

// StreamContext provides encoder state for monitoring and retry decisions.
type StreamContext interface {
	// Stream returns the stream configuration, or nil if removed.
	Stream(streamID string) *types.Stream
	// IsRunning reports whether the encoder is in running state.
	IsRunning() bool
}

// EventCallback handles stream event notifications.
type EventCallback func(streamID, streamName string, event string, message string, err string, retryCount, maxRetries int)

// Manager orchestrates multiple streams.
type Manager struct {
	ffmpegPath    string
	streams       map[string]*Stream
	mu            sync.RWMutex // Protects streams map
	onEvent       EventCallback
	getStreamName func(string) string
}

// Stream represents a managed SRT stream to a server.
type Stream struct {
	result     *ffmpeg.StartResult
	state      types.ProcessState
	lastError  string
	startTime  time.Time
	retryCount int
	backoff    *util.Backoff
}

// NewManager creates a Manager with the given FFmpeg path.
func NewManager(ffmpegPath string) *Manager {
	return &Manager{
		ffmpegPath: ffmpegPath,
		streams:    make(map[string]*Stream),
	}
}

// SetEventCallback configures the event handler and stream name resolver.
func (m *Manager) SetEventCallback(cb EventCallback, getStreamName func(string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvent = cb
	m.getStreamName = getStreamName
}

func (m *Manager) emitEvent(streamID, event, message, errMsg string, retryCount, maxRetries int) {
	m.mu.RLock()
	cb := m.onEvent
	getName := m.getStreamName
	m.mu.RUnlock()

	if cb == nil {
		return
	}

	var name string
	if getName != nil {
		name = getName(streamID)
	}
	cb(streamID, name, event, message, errMsg, retryCount, maxRetries)
}

// Start launches a stream. On success, a goroutine emits a "stream_stable"
// event after the stability threshold is reached.
func (m *Manager) Start(stream *types.Stream) error {
	m.mu.Lock()

	existing, exists := m.streams[stream.ID]
	if exists && existing.state == types.ProcessRunning {
		m.mu.Unlock()
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

	args := BuildFFmpegArgs(stream)

	slog.Info("starting stream", "stream_id", stream.ID, "host", stream.Host, "port", stream.Port)

	result, err := ffmpeg.StartProcess(m.ffmpegPath, args)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	s := &Stream{
		result:     result,
		state:      types.ProcessRunning,
		startTime:  time.Now(),
		retryCount: retryCount,
		backoff:    backoff,
	}

	m.streams[stream.ID] = s
	m.mu.Unlock()

	m.emitEvent(stream.ID, "stream_started", fmt.Sprintf("Connecting to %s:%d", stream.Host, stream.Port), "", 0, 0)

	// Emit stable event after threshold if still running
	go func(id string) {
		time.Sleep(types.StableThreshold)
		m.mu.RLock()
		s, exists := m.streams[id]
		isRunning := exists && s.state == types.ProcessRunning
		m.mu.RUnlock()
		if isRunning {
			m.emitEvent(id, "stream_stable", "Stream connected and stable", "", 0, 0)
		}
	}(stream.ID)

	return nil
}

// Stop terminates a stream with proper graceful shutdown.
func (m *Manager) Stop(streamID string) error {
	m.mu.Lock()
	stream, exists := m.streams[streamID]
	if !exists {
		m.mu.Unlock()
		return nil
	}

	if stream.state != types.ProcessRunning && stream.state != types.ProcessStarting {
		delete(m.streams, streamID)
		m.mu.Unlock()
		return nil
	}

	stream.state = types.ProcessStopping
	result := stream.result
	m.mu.Unlock()

	slog.Info("stopping stream", "stream_id", streamID)

	// Mark as intentionally stopped before closing stdin
	result.Cancel(errStoppedByUser)

	// Close stdin - signals FFmpeg that input is done
	result.CloseStdin()

	// Wait for process to exit with timeout
	done := make(chan error, 1)
	go func() {
		done <- result.Wait()
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(5 * time.Second):
		// Graceful shutdown: send SIGTERM
		slog.Warn("stream did not stop in time, sending signal", "stream_id", streamID)
		_ = result.Signal()

		select {
		case <-done:
			// Process stopped after signal
		case <-time.After(2 * time.Second):
			// Force kill if still running
			slog.Error("stream force killed", "stream_id", streamID)
			_ = result.Kill()
			<-done
		}
	}

	m.mu.Lock()
	delete(m.streams, streamID)
	m.mu.Unlock()

	return nil
}

// StopAll terminates all streams and returns any errors joined together.
func (m *Manager) StopAll() error {
	m.mu.RLock()
	ids := slices.Collect(maps.Keys(m.streams))
	m.mu.RUnlock()

	var errs []error
	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			slog.Error("failed to stop stream", "stream_id", id, "error", err)
			errs = append(errs, err)
		}
	}

	m.mu.Lock()
	clear(m.streams)
	m.mu.Unlock()

	return errors.Join(errs...)
}

// WriteAudio sends audio data to a stream. Errors from closed stdin during
// shutdown are silently ignored.
func (m *Manager) WriteAudio(streamID string, data []byte) error {
	// Get stream under read lock
	m.mu.RLock()
	stream, exists := m.streams[streamID]
	if !exists || stream.state != types.ProcessRunning {
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()

	// WriteStdin is thread-safe (mutex encapsulated in StartResult)
	_, err := stream.result.WriteStdin(data)
	if err != nil {
		// ErrStdinClosed is expected during shutdown - not a real error
		if errors.Is(err, ffmpeg.ErrStdinClosed) {
			return nil
		}

		// Update state under write lock on error
		m.mu.Lock()
		// Re-check stream still exists and hasn't been modified
		if stream, exists := m.streams[streamID]; exists && stream.state == types.ProcessRunning {
			slog.Warn("stream write failed, marking as error", "stream_id", streamID, "error", err)
			stream.state = types.ProcessError
			stream.result.CloseStdin()
		}
		m.mu.Unlock()
		return fmt.Errorf("write audio: %w", err)
	}
	return nil
}

// Statuses returns status information for all managed streams.
func (m *Manager) Statuses(getMaxRetries func(string) int) map[string]types.ProcessStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]types.ProcessStatus)
	for id, stream := range m.streams {
		maxRetries := getMaxRetries(id)
		isRunning := stream.state == types.ProcessRunning
		runDuration := time.Since(stream.startTime)

		var uptime string
		if isRunning {
			uptime = util.FormatDuration(runDuration.Milliseconds())
		}

		statuses[id] = types.ProcessStatus{
			State:      stream.state,
			Stable:     isRunning && runDuration >= types.StableThreshold,
			Exhausted:  stream.retryCount > maxRetries,
			RetryCount: stream.retryCount,
			MaxRetries: maxRetries,
			Error:      stream.lastError,
			Uptime:     uptime,
		}
	}
	return statuses
}

// StreamInfo returns the FFmpeg result and backoff state for a stream.
// The exists return value is false if the stream is not found.
func (m *Manager) StreamInfo(streamID string) (result *ffmpeg.StartResult, backoff *util.Backoff, exists bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, exists := m.streams[streamID]
	if !exists {
		return nil, nil, false
	}
	return stream.result, stream.backoff, true
}

// SetError records an error message and sets the stream state to error.
func (m *Manager) SetError(streamID, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, exists := m.streams[streamID]; exists {
		stream.lastError = errMsg
		stream.state = types.ProcessError
	}
}

// IncrementRetry advances the retry counter for a stream.
func (m *Manager) IncrementRetry(streamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, exists := m.streams[streamID]; exists {
		stream.retryCount++
	}
}

// ResetRetry clears the retry counter and backoff delay for a stream.
func (m *Manager) ResetRetry(streamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, exists := m.streams[streamID]; exists {
		stream.retryCount = 0
		if stream.backoff != nil {
			stream.backoff.Reset()
		}
	}
}

// MarkStopped updates the stream state to stopped without terminating the process.
func (m *Manager) MarkStopped(streamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, exists := m.streams[streamID]; exists {
		stream.state = types.ProcessStopped
	}
}

// Remove deletes a stream from the manager without stopping the process.
func (m *Manager) Remove(streamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streams, streamID)
}

// RetryCount returns the number of retry attempts for a stream, or 0 if not found.
func (m *Manager) RetryCount(streamID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if stream, exists := m.streams[streamID]; exists {
		return stream.retryCount
	}
	return 0
}

func (m *Manager) handleStreamExit(streamID string, result *ffmpeg.StartResult, backoff *util.Backoff, err error, runDuration time.Duration) {
	// Check if this was an intentional stop - don't treat as error
	cause := context.Cause(result.Context())
	if errors.Is(cause, errStoppedByUser) {
		m.emitEvent(streamID, "stream_stopped", "Stream stopped by user", "", 0, 0)
		return
	}

	if err != nil {
		errMsg := util.ExtractLastError(result.Stderr())
		if errMsg == "" {
			errMsg = err.Error()
		}
		if cause != nil {
			slog.Error("stream error", "stream_id", streamID, "error", errMsg, "cause", cause)
		} else {
			slog.Error("stream error", "stream_id", streamID, "error", errMsg)
		}
		m.SetError(streamID, errMsg)
		m.emitEvent(streamID, "stream_error", "Stream failed", errMsg, 0, 0)

		if runDuration >= types.SuccessThreshold {
			m.ResetRetry(streamID)
		} else {
			m.IncrementRetry(streamID)
			backoff.Next()
		}
	} else {
		m.ResetRetry(streamID)
		m.emitEvent(streamID, "stream_stopped", "Stream ended normally", "", 0, 0)
	}
}

func (m *Manager) shouldContinueRetry(streamID string, ctx StreamContext) (shouldRetry bool, reason string) {
	if !ctx.IsRunning() {
		return false, "encoder stopped"
	}
	stream := ctx.Stream(streamID)
	if stream == nil {
		return false, "stream removed"
	}
	if !stream.IsEnabled() {
		return false, "stream disabled"
	}
	retryCount := m.RetryCount(streamID)
	maxRetries := stream.MaxRetriesOrDefault()
	if retryCount > maxRetries {
		return false, "max retries exceeded"
	}
	return true, ""
}

// MonitorAndRetry watches a stream and restarts it on failure. This method
// blocks until the stream is stopped or retry limits are exceeded.
func (m *Manager) MonitorAndRetry(streamID string, ctx StreamContext, stopChan <-chan struct{}) {
	for {
		select {
		case <-stopChan:
			m.Remove(streamID)
			return
		default:
		}

		result, backoff, exists := m.StreamInfo(streamID)
		if !exists || result == nil || backoff == nil {
			return
		}

		startTime := time.Now()
		err := result.Wait()
		runDuration := time.Since(startTime)

		m.MarkStopped(streamID)
		m.handleStreamExit(streamID, result, backoff, err, runDuration)

		shouldRetry, reason := m.shouldContinueRetry(streamID, ctx)
		if !shouldRetry {
			if reason != "" {
				slog.Info("stream monitoring stopped", "stream_id", streamID, "reason", reason)
			}
			if reason != "max retries exceeded" {
				m.Remove(streamID)
			}
			return
		}

		retryDelay := backoff.Current()
		retryCount := m.RetryCount(streamID)
		stream := ctx.Stream(streamID)
		maxRetries := stream.MaxRetriesOrDefault()
		slog.Info("stream stopped, waiting before retry",
			"stream_id", streamID, "delay", retryDelay, "retry", retryCount, "max_retries", maxRetries)
		m.emitEvent(streamID, "stream_retry", fmt.Sprintf("Retrying in %s", retryDelay.Round(time.Second)), "", retryCount, maxRetries)

		select {
		case <-stopChan:
			m.Remove(streamID)
			return
		case <-time.After(retryDelay):
		}

		// Re-check conditions after wait
		shouldRetry, reason = m.shouldContinueRetry(streamID, ctx)
		if !shouldRetry {
			slog.Info("stream not restarting", "stream_id", streamID, "reason", reason)
			if reason != "max retries exceeded" {
				m.Remove(streamID)
			}
			return
		}

		stream = ctx.Stream(streamID)
		if err := m.Start(stream); err != nil {
			slog.Error("failed to restart stream", "stream_id", streamID, "error", err)
			m.Remove(streamID)
			return
		}
	}
}
