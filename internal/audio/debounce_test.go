package audio

import (
	"testing"
	"time"
)

func TestDebouncerEnterAndRecover(t *testing.T) {
	t.Parallel()
	var d debouncer
	base := time.Now()
	if r := d.update(true, 1000, 500, base); r.justEntered || r.active {
		t.Fatalf("entered too early: %+v", r)
	}
	r := d.update(true, 1000, 500, base.Add(1000*time.Millisecond))
	if !r.justEntered || !r.active || r.durationMs != 1000 {
		t.Fatalf("did not enter at duration threshold: %+v", r)
	}
	if r := d.update(true, 1000, 500, base.Add(2000*time.Millisecond)); !r.active || r.justEntered || r.durationMs != 2000 {
		t.Fatalf("steady active wrong: %+v", r)
	}
	if r := d.update(false, 1000, 500, base.Add(2200*time.Millisecond)); !r.active || r.justRecovered || r.durationMs != 0 {
		t.Fatalf("recovery window wrong: %+v", r)
	}
	r = d.update(false, 1000, 500, base.Add(2700*time.Millisecond))
	if !r.justRecovered || r.active {
		t.Fatalf("did not recover: %+v", r)
	}
	if r.totalDurationMs != 2000 || r.recoveryDurationMs != 500 {
		t.Fatalf("recovery totals wrong: totalDurationMs=%d recoveryDurationMs=%d", r.totalDurationMs, r.recoveryDurationMs)
	}
}

func TestDebouncerNoEntryWhenConditionDropsBeforeDuration(t *testing.T) {
	t.Parallel()
	var d debouncer
	base := time.Now()
	d.update(true, 1000, 500, base)                            // start accumulating
	d.update(false, 1000, 500, base.Add(500*time.Millisecond)) // drops before DurationMs
	if r := d.update(true, 1000, 500, base.Add(900*time.Millisecond)); r.justEntered || r.active {
		t.Fatalf("entered using a stale start time: %+v", r)
	}
}

func TestDebouncerRetriggerCancelsRecovery(t *testing.T) {
	t.Parallel()
	var d debouncer
	base := time.Now()
	d.update(true, 1000, 500, base)
	if r := d.update(true, 1000, 500, base.Add(1000*time.Millisecond)); !r.justEntered {
		t.Fatalf("did not confirm: %+v", r)
	}
	if r := d.update(false, 1000, 500, base.Add(1200*time.Millisecond)); !r.active || r.justRecovered {
		t.Fatalf("expected active recovery window: %+v", r)
	}
	r := d.update(true, 1000, 500, base.Add(1400*time.Millisecond))
	if !r.active || r.justEntered || r.justRecovered {
		t.Fatalf("re-trigger should stay active without re-entering: %+v", r)
	}
	if r.durationMs != 1400 {
		t.Fatalf("durationMs = %d, want 1400 (continues from original start)", r.durationMs)
	}
	if r := d.update(false, 1000, 500, base.Add(1700*time.Millisecond)); r.justRecovered || !r.active {
		t.Fatalf("stale recoveryStart caused premature recovery: %+v", r)
	}
}
func TestDebouncerResetClearsState(t *testing.T) {
	t.Parallel()
	var d debouncer
	base := time.Now()
	d.update(true, 1000, 500, base)
	d.update(true, 1000, 500, base.Add(1000*time.Millisecond)) // confirmed
	d.reset()
	if r := d.update(true, 1000, 500, base.Add(1100*time.Millisecond)); r.active || r.justEntered {
		t.Fatalf("state leaked after reset: %+v", r)
	}
}
