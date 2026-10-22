package recording

import (
	"bytes"
	"maps"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// This file exposes the few seams the black-box invariant corpus
// (invariants_test.go) needs beyond the Manager API. A redesign must keep
// offering them: a controllable clock for rotation and duration timers, a way
// to run the upload retry pass now, and child-process scenarios for crashes.

// ManualTimers replaces recorder timers (hourly rotation, on-demand duration
// limit) with timers that only fire through FireTimers, until tb ends. Call it
// before creating managers, so the hook is restored after their cleanup.
// Tests using it must not be parallel.
func ManualTimers(tb testing.TB) {
	previous := afterFunc
	manualTimers.Lock()
	manualTimers.armed = nil
	manualTimers.Unlock()
	afterFunc = func(d time.Duration, callback func()) *time.Timer {
		timer := time.NewTimer(math.MaxInt64)
		manualTimers.Lock()
		manualTimers.armed = append(manualTimers.armed, manualTimer{timer: timer, callback: callback, delay: d})
		manualTimers.Unlock()
		return timer
	}
	if tb != nil {
		tb.Cleanup(func() { afterFunc = previous })
	}
}

// FireTimers runs every timer that is still armed, as if its deadline passed,
// and waits until m's recorders have acted on them: a rotation has started the
// next file and finalized the previous one, a duration limit has stopped.
func FireTimers(m *Manager) {
	manualTimers.Lock()
	armed := manualTimers.armed
	manualTimers.armed = nil
	manualTimers.Unlock()
	for _, t := range armed {
		if t.timer.Stop() { // Still armed: the recorder did not cancel it.
			t.callback()
		}
	}
	Settle(m)
}

// Settle waits until m's recorders neither prepare nor finalize a file, such
// as the new file a configuration change starts.
func Settle(m *Manager) {
	m.mu.RLock()
	recorders := slices.Collect(maps.Values(m.recorders))
	m.mu.RUnlock()
	for _, r := range recorders {
		r.settle()
	}
}

// settle waits until r neither prepares nor finalizes a file.
func (r *GenericRecorder) settle() {
	if !WaitFor(10*time.Second, func() bool {
		idle := true
		return !r.do(func() { idle = r.run.next == nil && r.run.retiring == 0 }) || idle
	}) {
		panic("recorder " + r.id + " did not settle")
	}
}

type manualTimer struct {
	delay    time.Duration
	timer    *time.Timer
	callback func()
}

var manualTimers struct {
	sync.Mutex
	armed []manualTimer
}

func capturedTimers() []manualTimer {
	manualTimers.Lock()
	defer manualTimers.Unlock()
	return slices.Clone(manualTimers.armed)
}

// RetryUploadsNow makes every waiting upload due now, as if its backoff ended.
func RetryUploadsNow(m *Manager) { m.outbox.retryAllNow() }

// SetUploadClock replaces the clock the uploader judges backoff and overdue
// uploads by.
func SetUploadClock(m *Manager, now func() time.Time) {
	m.uploader.mu.Lock()
	m.uploader.now = now
	m.uploader.mu.Unlock()
}

// CorpusChildEnv names the child-process scenario the test binary runs instead
// of the tests; see TestMain.
const CorpusChildEnv = "ZWFM_RECORDING_CORPUS_CHILD"

// CorpusChildren holds child-process scenarios by name. A scenario runs in a
// re-executed test binary so the parent can kill it like a crashed service.
var CorpusChildren = map[string]func() int{}

// newTestRecorder creates a recorder with an outbox in its spool, as the
// Manager does; a second one on the same spool models a restart.
func newTestRecorder(t testing.TB, cfg GenericRecorderConfig) *GenericRecorder {
	t.Helper()
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = t.TempDir()
	}
	if cfg.Outbox == nil {
		ob, err := openOutbox(cfg.SpoolDir, func(*ticket) {})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Outbox = ob
	}
	r := NewGenericRecorder(cfg)
	t.Cleanup(r.close)
	return r
}

// Start starts r whatever its mode and enabled flag, as if the manager ran.
func (r *GenericRecorder) Start() error {
	err := errRecorderClosed
	r.do(func() {
		r.run.active = true
		err = r.startRun()
	})
	return err
}

// retryAllNow makes every waiting upload and cleanup due now, in memory only.
func (o *outbox) retryAllNow() {
	o.mu.Lock()
	for id := range o.tickets {
		if t := o.tickets[id]; t.State == ticketReady || t.State == ticketDelivered {
			t.NextAttempt = time.Time{}
			o.tickets[id] = t
		}
	}
	o.mu.Unlock()
	o.signal()
}

// inspect runs f on r's run state in its loop.
func (r *GenericRecorder) inspect(f func(l *run)) {
	r.do(func() { f(&r.run) })
}

// currentSegment returns the segment r is recording into, or nil.
func (r *GenericRecorder) currentSegment() *segment {
	var seg *segment
	r.inspect(func(l *run) { seg = l.seg })
	return seg
}

// currentFile returns the file r is recording into, or "".
func (r *GenericRecorder) currentFile() string {
	if seg := r.currentSegment(); seg != nil {
		return seg.file
	}
	return ""
}

// Stop finalizes the recording and waits for that; uploads are the uploader's,
// so Stop never waits for S3. A Stop already in progress is joined.
func (r *GenericRecorder) Stop() {
	stopped := closedChannel()
	r.do(func() { stopped = r.stop() })
	<-stopped
}

// UpdateConfig applies cfg and waits for a stop it causes.
func (r *GenericRecorder) UpdateConfig(cfg *types.Recorder) error {
	stopped, err := r.update(cfg)
	if err != nil {
		return err
	}
	<-stopped
	return nil
}

func (r *GenericRecorder) generateFilename(t time.Time) string {
	cfg := r.Config()
	return recordingFilename(&cfg, t)
}

// WaitFor polls cond until it holds or timeout passes.
func WaitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WriteAudio fans out one PCM copy without blocking the distributor.
func (m *Manager) WriteAudio(pcm []byte) {
	var shared []byte
	m.WriteAudioShared(func() []byte {
		if shared == nil {
			shared = bytes.Clone(pcm)
		}
		return shared
	})
}

// Start begins recorder management and is idempotent across source retries.
func (m *Manager) Start() error {
	return m.StartIfActive(nil)
}

// saveDestination stores the credentials of a destination.
func (o *outbox) saveDestination(id string, d S3Config) error {
	o.destMu.Lock()
	defer o.destMu.Unlock()
	return o.saveDestinationLocked(id, d)
}

// IsRunning reports whether recorder management is active.
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}
