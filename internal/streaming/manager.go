package streaming

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// errStoppedByUser indicates the stream was intentionally stopped.
var errStoppedByUser = errors.New("stopped by user")

// audioBufferSize is the number of audio chunks buffered per stream.
// At ~100ms per chunk, 5 chunks provides ~500ms of buffer.
const audioBufferSize = 5

const listenerStdoutBufferSize = 4 * 1024

// Variables so lifecycle tests can shorten escalation without changing production defaults.
var (
	encoderRunSignalTimeout = 5 * time.Second
	encoderRunKillTimeout   = 2 * time.Second
)

// StreamContext provides encoder state for monitoring and retry decisions.
type StreamContext interface {
	// Stream returns the stream configuration, or nil if removed.
	Stream(streamID string) *types.Stream
	// IsRunning reports whether the encoder is in running state.
	IsRunning() bool
}

// EventCallback handles stream event notifications.
type EventCallback func(
	streamID, streamName, mode string, event string, message string,
	err string, retryCount, maxRetries int,
)

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
	mode       types.StreamMode // snapshot of the launch-time mode, already defaulted via ModeOrDefault
	lastError  string
	startTime  time.Time
	retryCount int
	backoff    *util.Backoff
	audioCh    chan []byte
	closeOnce  sync.Once
	writerWg   sync.WaitGroup
	audioDrops atomic.Int64

	fanout         *srtfanout.Server
	encoderMu      sync.RWMutex
	encoder        *encoderRun
	encoderLastErr string
}

type encoderRun struct {
	result     *ffmpeg.StartResult
	audioCh    chan []byte
	closeOnce  sync.Once
	wg         sync.WaitGroup
	stdoutDone chan struct{}
	startedAt  time.Time
}

// closeAudioCh safely closes the audio channel exactly once.
// Nil-safe: placeholder entries have no audioCh.
func (s *Stream) closeAudioCh() {
	if s.audioCh == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.audioCh)
	})
}

func (r *encoderRun) closeAudioCh() {
	if r == nil || r.audioCh == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.audioCh)
	})
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
	m.emitEventWithMode(streamID, "", event, message, errMsg, retryCount, maxRetries)
}

func (m *Manager) emitEventWithMode(
	streamID string,
	mode types.StreamMode,
	event string,
	message string,
	errMsg string,
	retryCount int,
	maxRetries int,
) {
	m.mu.RLock()
	cb := m.onEvent
	getName := m.getStreamName
	modeText := ""
	if mode != "" {
		modeText = string(mode.OrDefault())
	} else if stream, exists := m.streams[streamID]; exists && stream.mode != "" {
		modeText = string(stream.mode.OrDefault())
	}
	m.mu.RUnlock()

	if cb == nil {
		return
	}

	var name string
	if getName != nil {
		name = getName(streamID)
	}
	cb(streamID, name, modeText, event, message, errMsg, retryCount, maxRetries)
}

// maybeEmitStable emits stream_stable only for the same running stream instance.
// Pointer identity protects fast restarts that reuse the stream ID.
func (m *Manager) maybeEmitStable(id string, started *Stream) {
	m.mu.RLock()
	cur, exists := m.streams[id]
	stable := exists && cur == started && cur.state == types.ProcessRunning
	m.mu.RUnlock()
	if stable {
		m.emitEvent(id, "stream_stable", "Stream connected and stable", "", 0, 0)
	}
}

// runWriter is the per-stream goroutine that drains audioCh and writes to FFmpeg stdin.
func (m *Manager) runWriter(streamID string, s *Stream) {
	defer s.writerWg.Done()

	for data := range s.audioCh {
		_, err := s.result.WriteStdin(data)
		if err == nil {
			continue
		}

		// ErrStdinClosed is expected during shutdown - not a real error
		if errors.Is(err, ffmpeg.ErrStdinClosed) {
			return
		}

		m.mu.Lock()
		if cur, exists := m.streams[streamID]; exists && cur == s && cur.state == types.ProcessRunning {
			slog.Warn("stream write failed, marking as error",
				"stream_id", streamID, "error", err)
			cur.state = types.ProcessError
			cur.lastError = err.Error()
			cur.result.CloseStdin()
		}
		m.mu.Unlock()
		return
	}
}

// Start launches a stream and reports whether it created a process.
//
// The returned bool is false with a nil error when another path already owns the
// stream or removed it during startup. Callers use it to avoid duplicate retry
// monitors. Successful starts schedule stream_stable through maybeEmitStable.
//
// A ProcessStarting placeholder is inserted into the map while the lock is
// released for old-writer cleanup and process startup. This prevents
// concurrent Start calls from launching duplicate FFmpeg processes while
// keeping the lock free for WriteAudio and Statuses on other streams.
func (m *Manager) Start(stream *types.Stream) (bool, error) {
	if stream.ModeOrDefault() == types.StreamModeListener {
		return m.startListenerFanout(stream)
	}
	return m.startCaller(stream)
}

func (m *Manager) startCaller(stream *types.Stream) (bool, error) {
	if err := stream.Validate(); err != nil {
		return false, err
	}

	m.mu.Lock()

	existing, exists := m.streams[stream.ID]
	if exists && (existing.state == types.ProcessRunning || existing.state == types.ProcessStarting) {
		m.mu.Unlock()
		return false, nil // Already running or being started
	}

	// Preserve retry state and capture old stream for writer cleanup
	var oldStream *Stream
	retryCount := 0
	backoff := util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay)
	if exists {
		oldStream = existing
		if existing.backoff != nil {
			retryCount = existing.retryCount
			backoff = existing.backoff
		}
	}

	// Insert placeholder to claim this stream ID. Carries retry state
	// so concurrent callers see the correct backoff. Has no result,
	// audioCh, or writer - those are created after StartProcess succeeds.
	placeholder := &Stream{
		state:      types.ProcessStarting,
		mode:       stream.ModeOrDefault(),
		retryCount: retryCount,
		backoff:    backoff,
	}
	m.streams[stream.ID] = placeholder
	m.mu.Unlock()

	// Clean up old writer goroutine outside the lock.
	// The writer's error path acquires m.mu - waiting while holding
	// the lock would deadlock.
	if oldStream != nil {
		m.stopStreamResources(stream.ID, oldStream)
	}

	args := BuildCallerArgs(stream)

	slog.Info("starting stream", "stream_id", stream.ID, "host", stream.Host, "port", stream.Port)

	result, err := ffmpeg.StartProcess(m.ffmpegPath, args)
	if err != nil {
		m.mu.Lock()
		if m.streams[stream.ID] == placeholder {
			delete(m.streams, stream.ID)
		}
		m.mu.Unlock()
		return false, err
	}

	s := &Stream{
		result:     result,
		state:      types.ProcessRunning,
		mode:       stream.ModeOrDefault(),
		startTime:  time.Now(),
		retryCount: retryCount,
		backoff:    backoff,
		audioCh:    make(chan []byte, audioBufferSize),
	}
	s.writerWg.Add(1)

	m.mu.Lock()
	if m.streams[stream.ID] != placeholder {
		// Placeholder was removed by Stop/Remove during startup.
		// Kill the process we just spawned and bail out.
		m.mu.Unlock()
		result.Cancel(errStoppedByUser)
		result.CloseStdin()
		_ = result.Wait()
		return false, nil
	}
	m.streams[stream.ID] = s
	m.mu.Unlock()

	go m.runWriter(stream.ID, s)

	m.emitEvent(stream.ID, "stream_started", fmt.Sprintf("Connecting to %s", stream.Endpoint()), "", 0, 0)

	// Guard the stable event against restarts during the stability window.
	go func() {
		time.Sleep(types.StableThreshold)
		m.maybeEmitStable(stream.ID, s)
	}()

	return true, nil
}

func (m *Manager) startListenerFanout(stream *types.Stream) (bool, error) {
	if err := stream.Validate(); err != nil {
		return false, err
	}

	m.mu.Lock()

	existing, exists := m.streams[stream.ID]
	if exists && (existing.state == types.ProcessRunning || existing.state == types.ProcessStarting) {
		m.mu.Unlock()
		return false, nil
	}

	var oldStream *Stream
	retryCount := 0
	backoff := util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay)
	if exists {
		oldStream = existing
		if existing.backoff != nil {
			retryCount = existing.retryCount
			backoff = existing.backoff
		}
	}

	placeholder := &Stream{
		state:      types.ProcessStarting,
		mode:       types.StreamModeListener,
		retryCount: retryCount,
		backoff:    backoff,
	}
	m.streams[stream.ID] = placeholder
	m.mu.Unlock()

	if oldStream != nil {
		m.stopStreamResources(stream.ID, oldStream)
	}

	fanout, err := srtfanout.NewServer(listenerFanoutConfig(stream))
	if err != nil {
		m.removePlaceholder(stream.ID, placeholder)
		return false, err
	}
	if err := fanout.Start(); err != nil {
		m.removePlaceholder(stream.ID, placeholder)
		return false, err
	}

	s := &Stream{
		state:      types.ProcessRunning,
		mode:       types.StreamModeListener,
		startTime:  time.Now(),
		retryCount: retryCount,
		backoff:    backoff,
		fanout:     fanout,
	}

	if err := m.startListenerEncoderRun(stream.ID, s, stream); err != nil {
		fanout.Shutdown()
		if waitErr := fanout.Wait(); waitErr != nil {
			slog.Warn("srt fanout shutdown after encoder start failure returned error",
				"stream_id", stream.ID, "error", waitErr)
		}
		m.removePlaceholder(stream.ID, placeholder)
		return false, err
	}

	m.mu.Lock()
	if m.streams[stream.ID] != placeholder {
		m.mu.Unlock()
		m.stopStreamResources(stream.ID, s)
		return false, nil
	}
	m.streams[stream.ID] = s
	m.mu.Unlock()

	m.emitEvent(stream.ID, "stream_started", fmt.Sprintf("Listening on %s", stream.Endpoint()), "", 0, 0)
	return true, nil
}

func (m *Manager) removePlaceholder(streamID string, placeholder *Stream) {
	m.mu.Lock()
	if m.streams[streamID] == placeholder {
		delete(m.streams, streamID)
	}
	m.mu.Unlock()
}

func (m *Manager) startListenerEncoderRun(
	streamID string, s *Stream, cfg *types.Stream,
) error {
	args := BuildListenerPipeArgs(cfg)

	slog.Info("starting listener encoder", "stream_id", streamID, "host", cfg.ListenerBindHost(), "port", cfg.Port)

	result, err := ffmpeg.StartProcessWithStdout(m.ffmpegPath, args)
	if err != nil {
		return err
	}
	if result.Stdout() == nil {
		result.Cancel(errStoppedByUser)
		result.CloseStdin()
		_ = result.Wait()
		return fmt.Errorf("listener encoder stdout pipe unavailable")
	}

	run := &encoderRun{
		result:     result,
		audioCh:    make(chan []byte, audioBufferSize),
		stdoutDone: make(chan struct{}),
		startedAt:  time.Now(),
	}
	run.wg.Add(2)

	s.encoderMu.Lock()
	s.encoder = run
	s.encoderLastErr = ""
	s.encoderMu.Unlock()

	go m.runListenerWriter(streamID, run)
	go m.runListenerStdoutReader(streamID, s, run)

	return nil
}

func (m *Manager) runListenerWriter(streamID string, run *encoderRun) {
	defer run.wg.Done()

	for data := range run.audioCh {
		_, err := run.result.WriteStdin(data)
		if err == nil {
			continue
		}
		if errors.Is(err, ffmpeg.ErrStdinClosed) {
			return
		}
		slog.Warn("listener encoder stdin write failed", "stream_id", streamID, "error", err)
		run.result.CloseStdin()
		return
	}
}

func (m *Manager) runListenerStdoutReader(streamID string, s *Stream, run *encoderRun) {
	defer run.wg.Done()
	defer close(run.stdoutDone)

	stdout := run.result.Stdout()
	if stdout == nil {
		return
	}

	buf := make([]byte, listenerStdoutBufferSize)
	for {
		n, err := stdout.Read(buf)
		if n > 0 && s.fanout != nil {
			s.fanout.Write(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(context.Cause(run.result.Context()), errStoppedByUser) {
				slog.Debug("listener encoder stdout read ended", "stream_id", streamID, "error", err)
			}
			return
		}
	}
}

func (m *Manager) stopStreamResources(streamID string, stream *Stream) {
	if stream == nil {
		return
	}
	if stream.mode == types.StreamModeListener {
		m.stopListenerResources(streamID, stream)
		return
	}
	stream.closeAudioCh()
	stream.writerWg.Wait()
}

func (m *Manager) stopListenerResources(streamID string, stream *Stream) {
	run := stream.detachEncoderRun(nil)
	m.stopEncoderRun(streamID, run)

	if stream.fanout != nil {
		stream.fanout.Shutdown()
		if err := stream.fanout.Wait(); err != nil {
			slog.Warn("srt fanout shutdown returned error", "stream_id", streamID, "error", err)
		}
	}
}

func (s *Stream) detachEncoderRun(expected *encoderRun) *encoderRun {
	s.encoderMu.Lock()
	defer s.encoderMu.Unlock()

	run := s.encoder
	if expected != nil && run != expected {
		return nil
	}
	if run != nil {
		run.closeAudioCh()
	}
	s.encoder = nil
	return run
}

func (m *Manager) stopEncoderRun(streamID string, run *encoderRun) {
	if run == nil {
		return
	}
	run.closeAudioCh()
	if run.result != nil {
		run.result.Cancel(errStoppedByUser)
		m.waitEncoderRunGoroutines(streamID, run)
		_ = run.result.Wait()
		run.result.CloseStdin()
		return
	}
	run.wg.Wait()
}

func (m *Manager) waitEncoderRunGoroutines(streamID string, run *encoderRun) {
	done := make(chan struct{})
	go func() {
		run.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(encoderRunSignalTimeout):
		slog.Warn("listener encoder did not stop in time, sending signal", "stream_id", streamID)
		_ = run.result.Signal()
	}

	select {
	case <-done:
		return
	case <-time.After(encoderRunKillTimeout):
		slog.Error("listener encoder force killed", "stream_id", streamID)
		_ = run.result.Kill()
	}

	<-done
}

// Stop terminates a stream with proper graceful shutdown.
func (m *Manager) Stop(streamID string) error {
	m.mu.Lock()
	stream, exists := m.streams[streamID]
	if !exists {
		m.mu.Unlock()
		return nil
	}

	// Placeholder: no process, audioCh, or writer to clean up.
	// Removing it signals the Start() goroutine to abort on re-check.
	if stream.state == types.ProcessStarting {
		delete(m.streams, streamID)
		m.mu.Unlock()
		return nil
	}

	if stream.mode == types.StreamModeListener {
		stream.state = types.ProcessStopping
		m.mu.Unlock()

		slog.Info("stopping listener stream", "stream_id", streamID)
		m.stopListenerResources(streamID, stream)
		m.emitEvent(streamID, "stream_stopped", "Stream stopped by user", "", 0, 0)

		m.mu.Lock()
		if m.streams[streamID] == stream {
			delete(m.streams, streamID)
		}
		m.mu.Unlock()

		return nil
	}

	if stream.state != types.ProcessRunning {
		delete(m.streams, streamID)
		m.mu.Unlock()

		// Clean up writer goroutine. This path is hit when MonitorAndRetry
		// sets ProcessStopped/ProcessError before StopAll reaches this stream.
		stream.closeAudioCh()
		stream.writerWg.Wait()

		return nil
	}

	stream.state = types.ProcessStopping
	result := stream.result
	m.mu.Unlock()

	slog.Info("stopping stream", "stream_id", streamID)

	// 1. Close audio channel - no more data from distributor.
	//    Writer's for-range will exit after draining remaining items.
	stream.closeAudioCh()

	// 2. Cancel context - marks stop as intentional. For stream processes
	//    (no cmd.Cancel set), exec.CommandContext sends SIGKILL, breaking
	//    the pipe and unblocking any writer stuck in stdin.Write().
	result.Cancel(errStoppedByUser)

	// 3. Wait for process exit with timeout escalation.
	//    Must complete before writerWg.Wait - guarantees the process is
	//    dead and the pipe is broken, so the writer can exit.
	done := make(chan error, 1)
	go func() {
		done <- result.Wait()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("stream did not stop in time, sending signal", "stream_id", streamID)
		_ = result.Signal()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Error("stream force killed", "stream_id", streamID)
			_ = result.Kill()
			<-done
		}
	}

	// 4. Writer goroutine - process is dead, pipe is broken,
	//    any blocked Write() has returned. This returns quickly.
	stream.writerWg.Wait()

	// 5. CloseStdin - best-effort cleanup. Writer released stdinMu,
	//    so this won't block. May be a no-op if writer already closed it.
	result.CloseStdin()

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

// WriteAudio enqueues audio data for a stream. The data is copied and sent
// to the stream's buffered channel for its writer goroutine. If the buffer
// is full, the oldest chunk is dropped to keep the most recent audio.
func (m *Manager) WriteAudio(streamID string, data []byte) error {
	m.mu.RLock()
	stream, exists := m.streams[streamID]
	if !exists || stream.state != types.ProcessRunning {
		m.mu.RUnlock()
		return nil
	}
	if stream.mode == types.StreamModeListener {
		m.mu.RUnlock()
		m.writeListenerAudio(streamID, stream, data)
		return nil
	}
	ch := stream.audioCh

	// Clone data because the caller reuses the buffer.
	buf := bytes.Clone(data)

	stream.offerAudio(streamID, ch, buf, "audio buffer full, dropping chunk")

	m.mu.RUnlock()
	return nil
}

func (m *Manager) writeListenerAudio(streamID string, stream *Stream, data []byte) {
	buf := bytes.Clone(data)

	stream.encoderMu.RLock()
	run := stream.encoder
	if run == nil || run.audioCh == nil {
		stream.encoderMu.RUnlock()
		return
	}
	ch := run.audioCh
	stream.offerAudio(streamID, ch, buf, "listener encoder audio buffer full, dropping chunk")
	stream.encoderMu.RUnlock()
}

func (s *Stream) offerAudio(streamID string, ch chan []byte, buf []byte, logMessage string) {
	// Each stream audio channel has one producer. Concurrent producers are
	// memory-safe, but drop accounting may be lossy under contention.
	// Drop-oldest: if full, discard one stale chunk, then enqueue fresh.
	// Inner non-blocking selects handle a concurrent writer drain.
	select {
	case ch <- buf:
	default:
		select {
		case <-ch:
			drops := s.audioDrops.Add(1)
			if drops == 1 || drops%100 == 0 {
				slog.Warn(logMessage,
					"stream_id", streamID, "total_drops", drops)
			}
		default:
		}
		select {
		case ch <- buf:
		default:
		}
	}
}

// Statuses returns status information for all managed streams.
func (m *Manager) Statuses(getStreamConfig func(string) *types.Stream) map[string]types.ProcessStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]types.ProcessStatus)
	for id, stream := range m.streams {
		cfg := getStreamConfig(id)
		maxRetries := types.DefaultMaxRetries
		if cfg != nil {
			maxRetries = cfg.MaxRetriesOrDefault()
		}
		isRunning := stream.state == types.ProcessRunning
		runDuration := time.Since(stream.startTime)
		isListener := stream.mode == types.StreamModeListener
		encoderRunning := false
		clientCount := int64(0)
		listenerDrops := int64(0)
		errMsg := stream.lastError
		if isListener {
			stream.encoderMu.RLock()
			encoderRunning = stream.encoder != nil
			if stream.encoderLastErr != "" {
				errMsg = stream.encoderLastErr
			}
			stream.encoderMu.RUnlock()
			if stream.fanout != nil {
				clientCount = stream.fanout.ClientCount()
				listenerDrops = stream.fanout.DropCount()
			}
		}

		var uptime string
		if isRunning {
			uptime = util.FormatDuration(runDuration.Milliseconds())
		}

		statuses[id] = types.ProcessStatus{
			State:          stream.state,
			Stable:         isRunning && !isListener && runDuration >= types.StableThreshold,
			Exhausted:      stream.retryCount > maxRetries,
			RetryCount:     stream.retryCount,
			MaxRetries:     maxRetries,
			Error:          errMsg,
			Uptime:         uptime,
			AudioDrops:     stream.audioDrops.Load(),
			EncoderRunning: isListener && encoderRunning,
			ClientCount:    clientCount,
			ListenerDrops:  listenerDrops,
		}
	}
	return statuses
}

// StreamInfo returns the process result, retry backoff, and mode for streamID.
// The exists return value is false if the stream is not found.
func (m *Manager) StreamInfo(
	streamID string,
) (result *ffmpeg.StartResult, backoff *util.Backoff, mode types.StreamMode, exists bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, exists := m.streams[streamID]
	if !exists {
		return nil, nil, types.StreamModeCaller, false
	}
	return stream.result, stream.backoff, stream.mode, true
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

// Remove deletes a stream from the manager and cleans up its writer goroutine.
func (m *Manager) Remove(streamID string) {
	m.mu.Lock()
	stream, exists := m.streams[streamID]
	if exists {
		delete(m.streams, streamID)
	}
	m.mu.Unlock()

	// Close channel and wait for writer AFTER releasing m.mu.
	// The writer's error path acquires m.mu - waiting while holding
	// the lock would deadlock.
	if exists {
		m.stopStreamResources(streamID, stream)
	}
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

type streamExitClass int

const (
	streamExitNormalStop streamExitClass = iota
	streamExitIntentionalStop
	streamExitFailure
)

func classifyStreamExit(
	mode types.StreamMode, err error, cause error,
) streamExitClass {
	if errors.Is(cause, errStoppedByUser) {
		return streamExitIntentionalStop
	}
	if mode.OrDefault() == types.StreamModeListener {
		return streamExitFailure
	}
	if err != nil {
		return streamExitFailure
	}
	return streamExitNormalStop
}

func resolveExitError(result *ffmpeg.StartResult, err error) string {
	if err == nil {
		return ""
	}
	if result != nil {
		if errMsg := util.ExtractLastError(result.Stderr()); errMsg != "" {
			return errMsg
		}
	}
	return err.Error()
}

func (m *Manager) handleStreamExit(
	streamID string, result *ffmpeg.StartResult, backoff *util.Backoff, mode types.StreamMode,
	err error, runDuration time.Duration,
) {
	cause := context.Cause(result.Context())

	switch classifyStreamExit(mode, err, cause) {
	case streamExitIntentionalStop:
		m.emitEventWithMode(streamID, mode, "stream_stopped", "Stream stopped by user", "", 0, 0)
		return
	case streamExitNormalStop:
		m.ResetRetry(streamID)
		m.emitEventWithMode(streamID, mode, "stream_stopped", "Stream ended normally", "", 0, 0)
		return
	case streamExitFailure:
		// Handled by the shared error path below.
	}

	if err != nil {
		errMsg := resolveExitError(result, err)
		if cause != nil {
			slog.Error("stream error", "stream_id", streamID, "error", errMsg, "cause", cause)
		} else {
			slog.Error("stream error", "stream_id", streamID, "error", errMsg)
		}
		m.SetError(streamID, errMsg)
		m.emitEventWithMode(streamID, mode, "stream_error", "Stream failed", errMsg, 0, 0)

		if runDuration >= types.SuccessThreshold {
			m.ResetRetry(streamID)
		} else {
			m.IncrementRetry(streamID)
			backoff.Next()
		}
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
	if !stream.Enabled {
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
			return
		default:
		}

		result, backoff, mode, exists := m.StreamInfo(streamID)
		if exists && mode == types.StreamModeListener {
			m.monitorListenerEncoder(streamID, ctx, stopChan)
			return
		}
		if !exists || result == nil || backoff == nil {
			return
		}

		startTime := time.Now()
		err := result.Wait()
		runDuration := time.Since(startTime)

		m.MarkStopped(streamID)
		m.handleStreamExit(streamID, result, backoff, mode, err, runDuration)

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
		stream := ctx.Stream(streamID)
		retryCount := m.RetryCount(streamID)
		maxRetries := stream.MaxRetriesOrDefault()
		slog.Info("stream stopped, waiting before retry",
			"stream_id", streamID, "delay", retryDelay, "retry", retryCount, "max_retries", maxRetries)
		m.emitEventWithMode(streamID, mode, "stream_retry",
			fmt.Sprintf("Retrying in %s", retryDelay.Round(time.Second)),
			"", retryCount, maxRetries)

		select {
		case <-stopChan:
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
		started, err := m.Start(stream)
		if err != nil {
			slog.Error("failed to restart stream", "stream_id", streamID, "error", err)
			m.Remove(streamID)
			return
		}
		if !started {
			// The new owner already has a monitor.
			slog.Info("stream already restarted by another path, stopping duplicate monitor",
				"stream_id", streamID)
			return
		}
	}
}

func (m *Manager) monitorListenerEncoder(streamID string, ctx StreamContext, stopChan <-chan struct{}) {
	for {
		select {
		case <-stopChan:
			return
		default:
		}

		stream, run, backoff, exists := m.listenerEncoderInfo(streamID)
		if !exists || stream == nil || backoff == nil || run == nil {
			return
		}

		select {
		case <-stopChan:
			return
		case <-run.stdoutDone:
		}

		if detached := stream.detachEncoderRun(run); detached == nil {
			return
		}
		m.waitEncoderRunGoroutines(streamID, run)
		run.result.CloseStdin()
		err := run.result.Wait()
		runDuration := time.Since(run.startedAt)
		cause := context.Cause(run.result.Context())

		if errors.Is(cause, errStoppedByUser) {
			return
		}

		errMsg := resolveExitError(run.result, err)
		if errMsg == "" {
			errMsg = "listener encoder exited"
		}
		m.recordListenerEncoderFailure(streamID, stream, backoff, errMsg, runDuration)

		for {
			cfg, ok := m.prepareListenerRetry(streamID, stream, ctx, errMsg)
			if !ok {
				return
			}

			retryDelay := backoff.Current()
			retryCount := m.RetryCount(streamID)
			maxRetries := cfg.MaxRetriesOrDefault()
			slog.Info("listener encoder stopped, waiting before retry",
				"stream_id", streamID, "delay", retryDelay, "retry", retryCount, "max_retries", maxRetries)
			m.emitEventWithMode(streamID, types.StreamModeListener, "stream_retry",
				fmt.Sprintf("Retrying encoder in %s", retryDelay.Round(time.Second)),
				"", retryCount, maxRetries)

			select {
			case <-stopChan:
				return
			case <-time.After(retryDelay):
			}

			cfg, ok = m.prepareListenerRetry(streamID, stream, ctx, errMsg)
			if !ok {
				return
			}
			if err := m.startListenerEncoderRun(streamID, stream, cfg); err != nil {
				errMsg = err.Error()
				slog.Error("failed to restart listener encoder", "stream_id", streamID, "error", err)
				m.recordListenerEncoderFailure(streamID, stream, backoff, errMsg, 0)
				continue
			}
			break
		}
	}
}

// prepareListenerRetry prepares the listener encoder to attempt another run.
// When retry is no longer permitted it finalizes shutdown via
// stopListenerAfterRetryEnd and returns a false ok value; otherwise it returns the
// current stream config.
func (m *Manager) prepareListenerRetry(
	streamID string, stream *Stream, ctx StreamContext, errMsg string,
) (cfg *types.Stream, ok bool) {
	shouldRetry, reason := m.shouldContinueRetry(streamID, ctx)
	if !shouldRetry {
		m.stopListenerAfterRetryEnd(streamID, stream, reason, errMsg)
		return nil, false
	}
	cfg = ctx.Stream(streamID)
	if cfg == nil {
		m.stopListenerAfterRetryEnd(streamID, stream, "stream removed", errMsg)
		return nil, false
	}
	return cfg, true
}

func (m *Manager) listenerEncoderInfo(
	streamID string,
) (stream *Stream, run *encoderRun, backoff *util.Backoff, exists bool) {
	m.mu.RLock()
	stream, exists = m.streams[streamID]
	if !exists || stream.mode != types.StreamModeListener {
		m.mu.RUnlock()
		return nil, nil, nil, false
	}
	backoff = stream.backoff
	m.mu.RUnlock()

	stream.encoderMu.RLock()
	run = stream.encoder
	stream.encoderMu.RUnlock()
	return stream, run, backoff, true
}

func (m *Manager) recordListenerEncoderFailure(
	streamID string, stream *Stream, backoff *util.Backoff, errMsg string, runDuration time.Duration,
) {
	stream.encoderMu.Lock()
	stream.encoderLastErr = errMsg
	stream.encoderMu.Unlock()

	slog.Error("listener encoder error", "stream_id", streamID, "error", errMsg)
	m.emitEventWithMode(streamID, stream.mode, "stream_error", "Listener encoder failed", errMsg, 0, 0)

	m.mu.Lock()
	if cur, exists := m.streams[streamID]; exists && cur == stream {
		if runDuration >= types.SuccessThreshold {
			cur.retryCount = 0
			if backoff != nil {
				backoff.Reset()
			}
		} else {
			cur.retryCount++
			if backoff != nil {
				backoff.Next()
			}
		}
	}
	m.mu.Unlock()
}

func (m *Manager) stopListenerAfterRetryEnd(streamID string, stream *Stream, reason, errMsg string) {
	if reason != "" {
		slog.Info("listener encoder monitoring stopped", "stream_id", streamID, "reason", reason)
	}

	if reason == "max retries exceeded" {
		m.mu.Lock()
		if cur, exists := m.streams[streamID]; exists && cur == stream {
			cur.state = types.ProcessError
			cur.lastError = errMsg
		}
		m.mu.Unlock()

		if stream.fanout != nil {
			stream.fanout.Shutdown()
			if err := stream.fanout.Wait(); err != nil {
				slog.Warn("srt fanout shutdown after retry exhaustion returned error",
					"stream_id", streamID, "error", err)
			}
		}
		return
	}

	m.Remove(streamID)
}
