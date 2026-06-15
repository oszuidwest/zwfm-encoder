package audio

import "time"

// debouncer is the shared enter-after-duration / clear-after-recovery state
// machine behind [SilenceDetector] and [ImbalanceDetector]. It tracks how long an
// instantaneous condition has held and applies hysteresis: a confirmed state is
// entered after the condition stays true for DurationMs and cleared after it
// stays false for RecoveryMs. The condition start is preserved during the
// recovery window (no hard reset), so a condition that drops away mid-period
// still produces a clean recovery rather than a dangling entry.
//
// It carries no mutex of its own; callers serialize access (the detectors hold
// their own lock around update/reset).
type debouncer struct {
	start            time.Time // when the current active period started
	recoveryStart    time.Time // when the condition cleared during recovery
	active           bool      // currently in confirmed active state
	activeDurationMs int64     // last measured active duration, retained for recovery reporting
}

// debounceResult is the timing outcome of a single [debouncer.update]. Detectors
// map it onto their domain-specific event, adding levels and a "level" label.
type debounceResult struct {
	active             bool  // in confirmed active state (including during the recovery window)
	justEntered        bool  // confirmed state was entered on this update
	justRecovered      bool  // confirmed state was cleared on this update
	durationMs         int64 // active duration so far (0 once recovery has begun)
	totalDurationMs    int64 // total active duration, reported on recovery
	recoveryDurationMs int64 // elapsed recovery time, reported on recovery
}

// update advances the state machine. conditionActive is the instantaneous
// trigger; durationMs and recoveryMs are the enter and clear thresholds.
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
			// Already in confirmed active state.
			r.active = true
			r.durationMs = elapsed
		} else if elapsed >= durationMs {
			// Just crossed the duration threshold - enter active state.
			d.active = true
			r.active = true
			r.durationMs = elapsed
			r.justEntered = true
		}
		return r
	}

	// Condition is false - preserve the start during recovery.
	if !d.active {
		d.start = time.Time{}
		return r
	}

	// Was active, condition cleared - check recovery.
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

	// Still in the recovery window - remain in confirmed active state.
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
