package audio

import "time"

// debouncer applies duration/recovery hysteresis for silence and imbalance detectors.
// It preserves the original start through recovery so one confirmed event keeps
// one duration even when the condition briefly clears.
//
// Callers must serialize access.
type debouncer struct {
	start            time.Time // current active period start
	recoveryStart    time.Time // recovery-window start
	active           bool      // confirmed active state
	activeDurationMs int64     // last active duration for recovery reporting
}

// debounceResult describes one transition from [debouncer.update].
type debounceResult struct {
	active             bool  // confirmed active state, including recovery
	justEntered        bool  // active state entered on this update
	justRecovered      bool  // active state cleared on this update
	durationMs         int64 // active duration, or 0 during recovery
	totalDurationMs    int64 // final active duration on recovery
	recoveryDurationMs int64 // elapsed recovery time on recovery
}

// update applies the instantaneous condition and returns transition flags.
func (d *debouncer) update(conditionActive bool, durationMs, recoveryMs int64, now time.Time) debounceResult {
	var r debounceResult

	if conditionActive {
		d.recoveryStart = time.Time{}

		if d.start.IsZero() {
			d.start = now
		}

		elapsed := now.Sub(d.start).Milliseconds()
		d.activeDurationMs = elapsed

		if d.active {
			r.active = true
			r.durationMs = elapsed
		} else if elapsed >= durationMs {
			d.active = true
			r.active = true
			r.durationMs = elapsed
			r.justEntered = true
		}
		return r
	}

	if !d.active {
		d.start = time.Time{}
		return r
	}

	// Keep start during recovery so totalDurationMs spans the whole active event.
	if d.recoveryStart.IsZero() {
		d.recoveryStart = now
	}

	recoveryElapsed := now.Sub(d.recoveryStart).Milliseconds()
	if recoveryElapsed >= recoveryMs {
		r.justRecovered = true
		r.totalDurationMs = d.activeDurationMs
		r.recoveryDurationMs = recoveryElapsed

		d.active = false
		d.activeDurationMs = 0
		d.start = time.Time{}
		d.recoveryStart = time.Time{}
		return r
	}

	r.active = true
	return r
}

// reset clears all debounce state.
func (d *debouncer) reset() {
	d.start = time.Time{}
	d.recoveryStart = time.Time{}
	d.active = false
	d.activeDurationMs = 0
}
