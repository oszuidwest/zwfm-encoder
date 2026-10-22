package recording

import (
	"math"
	"sync"
	"testing"
	"time"
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
	afterFunc = func(_ time.Duration, callback func()) *time.Timer {
		timer := time.NewTimer(math.MaxInt64)
		manualTimers.Lock()
		manualTimers.armed = append(manualTimers.armed, manualTimer{timer: timer, callback: callback})
		manualTimers.Unlock()
		return timer
	}
	if tb != nil {
		tb.Cleanup(func() { afterFunc = previous })
	}
}

// FireTimers runs every timer that is still armed, as if its deadline passed,
// and waits for the callbacks to return.
func FireTimers() {
	manualTimers.Lock()
	armed := manualTimers.armed
	manualTimers.armed = nil
	manualTimers.Unlock()
	var wg sync.WaitGroup
	for _, t := range armed {
		if t.timer.Stop() { // Still armed: the recorder did not cancel it.
			wg.Go(t.callback)
		}
	}
	wg.Wait()
}

type manualTimer struct {
	timer    *time.Timer
	callback func()
}

var manualTimers struct {
	sync.Mutex
	armed []manualTimer
}

// RetryUploadsNow runs the upload retry pass the hourly scheduler would run.
func RetryUploadsNow(m *Manager) { m.processUploadRetryQueues() }

// CorpusChildEnv names the child-process scenario the test binary runs instead
// of the tests; see TestMain.
const CorpusChildEnv = "ZWFM_RECORDING_CORPUS_CHILD"

// CorpusChildren holds child-process scenarios by name. A scenario runs in a
// re-executed test binary so the parent can kill it like a crashed service.
var CorpusChildren = map[string]func() int{}
