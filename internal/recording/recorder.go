package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// errRecorderClosed is returned by commands sent after the recorder was removed.
var errRecorderClosed = errors.New("recorder was removed")

const recorderCloseTimeout = 10 * time.Second

// GenericRecorder saves audio to files with optional S3 upload. One loop
// goroutine owns the run: the methods send it commands, and blocking work
// (preparing a file, starting FFmpeg, finalizing) runs in helpers that report
// back to the loop. Status and Config read snapshots the loop publishes.
type GenericRecorder struct {
	id         string
	ffmpegPath string
	spoolDir   string
	outbox     *outbox
	events     eventWriter

	inbox     chan func()   // Work for the loop, run in order.
	quit      chan struct{} // Closed by close.
	exited    chan struct{} // Closed when the loop returns.
	closeOnce sync.Once
	helpers   sync.WaitGroup // Helpers the loop started; close waits for them.

	run run // Owned by the loop goroutine.

	status atomic.Pointer[types.ProcessStatus]
	config atomic.Pointer[types.Recorder]

	// Capture path: WriteAudio never waits for the loop.
	audioMu    sync.RWMutex // Protects audioCh from capture/close; never held across I/O.
	audioCh    chan []byte
	recording  atomic.Bool
	audioDrops atomic.Int64 // Lost chunks from overflow or teardown.
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

// NewGenericRecorder creates a recorder and starts its loop; close ends it.
func NewGenericRecorder(cfg GenericRecorderConfig) *GenericRecorder {
	recorder := *cfg.Recorder
	r := &GenericRecorder{
		id:         cfg.Recorder.ID,
		ffmpegPath: cfg.FFmpegPath,
		spoolDir:   cfg.SpoolDir,
		outbox:     cfg.Outbox,
		events:     eventWriter{logger: cfg.EventLogger},
		inbox:      make(chan func()),
		quit:       make(chan struct{}),
		exited:     make(chan struct{}),
		run: run{
			cfg:           recorder,
			maxMinutes:    cfg.MaxDurationMinutes,
			state:         types.ProcessStopped,
			stopped:       make(chan struct{}),
			usedFilenames: make(map[string]time.Time),
			prepareLocks:  make(map[string]*sync.Mutex),
		},
	}
	close(r.run.stopped)
	r.config.Store(&recorder)
	r.publishStatus()
	go r.loop()
	return r
}

func (r *GenericRecorder) loop() {
	defer close(r.exited)
	for {
		select {
		case f := <-r.inbox:
			f()
			r.publishStatus()
		case <-r.quit:
			// close gave up waiting: release other waiters of the stop too,
			// while the helpers finish on their own.
			if r.run.state == types.ProcessStopping {
				close(r.run.stopped)
			}
			return
		}
	}
}

// post queues f for the loop; false means the loop has ended.
func (r *GenericRecorder) post(f func()) bool {
	select {
	case r.inbox <- f:
		return true
	case <-r.exited:
		return false
	}
}

// do runs f in the loop and waits for it; false means the loop has ended.
// The caller then sees the status f left.
func (r *GenericRecorder) do(f func()) bool {
	done := make(chan struct{})
	if !r.post(func() { f(); r.publishStatus(); close(done) }) {
		return false
	}
	<-done
	return true
}

func (r *GenericRecorder) publishStatus() {
	r.status.Store(&types.ProcessStatus{State: r.run.state, Error: r.run.err})
}

// close stops the recorder, ends its loop and waits for its helpers and
// events, all within recorderCloseTimeout: a helper hung on an unresponsive
// share must not hold up shutdown.
func (r *GenericRecorder) close() {
	r.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), recorderCloseTimeout)
		defer cancel()
		stopped := r.deactivate()
		select {
		case <-stopped:
		case <-ctx.Done():
		}
		close(r.quit)
		<-r.exited
		drained := make(chan struct{})
		go func() { r.helpers.Wait(); r.events.flush(); close(drained) }()
		select {
		case <-drained:
		case <-ctx.Done():
			slog.Warn("recorder did not finish stopping in time", "id", r.id)
		}
	})
}

// Config returns a copy of the recorder configuration.
func (r *GenericRecorder) Config() types.Recorder {
	return *r.config.Load()
}

// Status returns the recorder process status.
func (r *GenericRecorder) Status() types.ProcessStatus {
	status := *r.status.Load()
	status.AudioDrops = r.audioDrops.Load()
	return status
}

// IsRecording reports whether recording is currently in progress.
func (r *GenericRecorder) IsRecording() bool {
	return r.recording.Load()
}

// activate lets the recorder record and starts it when it is enabled and
// hourly; the manager calls it when capture starts.
func (r *GenericRecorder) activate() error {
	err := errRecorderClosed
	r.do(func() {
		r.run.active = true
		r.startHourly()
		err = nil
	})
	return err
}

// deactivate stops the recorder and refuses starts until the next activate.
func (r *GenericRecorder) deactivate() <-chan struct{} {
	stopped := closedChannel()
	r.do(func() {
		r.run.active = false
		stopped = r.stop()
	})
	return stopped
}

// startOnDemand starts an enabled on-demand recorder while the manager runs.
func (r *GenericRecorder) startOnDemand() error {
	err := errRecorderClosed
	r.do(func() {
		cfg := &r.run.cfg
		switch {
		case !r.run.active:
			err = ErrManagerNotRunning
		case cfg.RecordingMode == types.RecordingHourly:
			err = ErrHourlyRecorderNotControllable
		case !cfg.Enabled:
			err = ErrRecorderDisabled
		default:
			err = r.startRun()
		}
	})
	return err
}

// stopOnDemand stops a running or starting on-demand recorder and waits for
// its recording to be finalized.
func (r *GenericRecorder) stopOnDemand() error {
	err := errRecorderClosed
	var stopped <-chan struct{}
	r.do(func() {
		switch {
		case r.run.cfg.RecordingMode == types.RecordingHourly:
			err = ErrHourlyRecorderNotControllable
		case r.run.state != types.ProcessRunning && r.run.state != types.ProcessStarting:
			err = ErrNotRecording
		default:
			err, stopped = nil, r.stop()
		}
	})
	if stopped != nil {
		<-stopped
	}
	return err
}

// update applies cfg. The returned channel closes when a stop the update
// caused (disabling, or clearing a failed on-demand run) is done.
func (r *GenericRecorder) update(cfg *types.Recorder) (<-chan struct{}, error) {
	if cfg.ID != r.id {
		return nil, fmt.Errorf("recorder ID cannot change")
	}
	stopped := closedChannel()
	if !r.do(func() { stopped = r.applyConfig(cfg) }) {
		return nil, errRecorderClosed
	}
	return stopped, nil
}

func closedChannel() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// WriteAudio queues pcm for non-blocking FFmpeg stdin writes.
// The caller must not mutate pcm after the call. Full buffers drop the newest
// chunk and increment AudioDrops.
func (r *GenericRecorder) WriteAudio(pcm []byte) {
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

// setAudio hands capture to ch (nil stops it) and closes the channel it
// replaces, so that segment's writer drains and stops filling silence (D1).
func (r *GenericRecorder) setAudio(ch chan []byte) {
	r.audioMu.Lock()
	old := r.audioCh
	r.audioCh = ch
	r.audioMu.Unlock()
	if old != nil {
		close(old) // No sender holds it once audioMu was taken.
	}
}

func recordingFilename(cfg *types.Recorder, t time.Time) string {
	return fmt.Sprintf("%s-%s.%s", cfg.FilePrefix, t.Format(recordingTimestampLayout), cfg.Codec.FileExtension())
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

// s3Namespace is the object-key prefix for files under one filename prefix.
func s3Namespace(filePrefix string) string {
	return "recordings/" + filePrefix + "/"
}

// eventWriter writes recorder events in order, outside the loop, so a slow
// event log never holds up the recorder (#486).
type eventWriter struct {
	logger *eventlog.Logger
	mu     sync.Mutex
	queue  []*eventlog.Event
	busy   chan struct{} // Closed when the writer goroutine has emptied the queue.
}

func (w *eventWriter) emit(typ eventlog.EventType, details *eventlog.RecorderDetails) {
	if w.logger == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queue = append(w.queue, &eventlog.Event{Type: typ, Details: details})
	if w.busy == nil {
		w.busy = make(chan struct{})
		go w.write(w.busy)
	}
}

func (w *eventWriter) write(busy chan struct{}) {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.busy = nil
			w.mu.Unlock()
			close(busy)
			return
		}
		e := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()
		if err := w.logger.Log(e); err != nil {
			slog.Warn("failed to log recorder event", "type", e.Type, "error", err)
		}
	}
}

// flush waits until the events emitted so far are written.
func (w *eventWriter) flush() {
	w.mu.Lock()
	busy := w.busy
	w.mu.Unlock()
	if busy != nil {
		<-busy
	}
}
