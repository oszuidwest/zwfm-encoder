package audio

import (
	"testing"
	"time"
)

func testImbalanceConfig() ImbalanceConfig {
	return ImbalanceConfig{
		ThresholdDB:     12,
		DurationMs:      15000,
		RecoveryMs:      5000,
		PresenceFloorDB: -40,
	}
}

func TestImbalanceDetectorEntersAfterDuration(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// Imbalanced (20 dB apart) but not yet long enough.
	e := d.Update(-10, -30, cfg, base)
	if e.JustEntered || e.InImbalance {
		t.Fatalf("entered too early: %+v", e)
	}
	if e.ImbalanceDB != 20 || e.BalanceDB != 20 {
		t.Fatalf("ImbalanceDB/BalanceDB = %v/%v, want 20/20 (left louder)", e.ImbalanceDB, e.BalanceDB)
	}

	// One millisecond before the duration threshold: still not confirmed.
	if e := d.Update(-10, -30, cfg, base.Add(14999*time.Millisecond)); e.JustEntered {
		t.Fatalf("entered before duration elapsed: %+v", e)
	}

	// At the duration threshold: enter confirmed imbalance.
	e = d.Update(-10, -30, cfg, base.Add(15000*time.Millisecond))
	if !e.JustEntered || !e.InImbalance || e.Level != ImbalanceLevelActive {
		t.Fatalf("did not enter at duration threshold: %+v", e)
	}
	if e.DurationMs != 15000 {
		t.Fatalf("DurationMs = %d, want 15000", e.DurationMs)
	}
}

func TestImbalanceDetectorRecoversAfterRecovery(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	d.Update(-10, -30, cfg, base)
	d.Update(-10, -30, cfg, base.Add(15000*time.Millisecond)) // confirmed

	// Channels balanced again, recovery window not yet elapsed.
	if e := d.Update(-10, -11, cfg, base.Add(16000*time.Millisecond)); e.JustRecovered || !e.InImbalance {
		t.Fatalf("recovered too early: %+v", e)
	}

	// Recovery window elapsed: imbalance clears.
	e := d.Update(-10, -11, cfg, base.Add(21000*time.Millisecond))
	if !e.JustRecovered {
		t.Fatalf("did not recover after recovery window: %+v", e)
	}
	if e.TotalDurationMs == 0 {
		t.Fatalf("TotalDurationMs = 0, want the confirmed imbalance duration")
	}
}

func TestImbalanceDetectorDeadChannelTriggers(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// Right channel dead (clamped to -60), left at -10: 50 dB apart.
	if e := d.Update(-10, -60, cfg, base); e.ImbalanceDB != 50 {
		t.Fatalf("ImbalanceDB = %v, want 50 for a dead right channel", e.ImbalanceDB)
	}
	e := d.Update(-10, -60, cfg, base.Add(15000*time.Millisecond))
	if !e.JustEntered || !e.InImbalance {
		t.Fatalf("dead channel did not trigger imbalance: %+v", e)
	}
}

func TestImbalanceDetectorSignedBalanceReflectsLouderChannel(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// Left louder -> positive balance.
	if e := d.Update(-10, -30, cfg, base); e.BalanceDB != 20 || e.ImbalanceDB != 20 {
		t.Fatalf("left louder: BalanceDB/ImbalanceDB = %v/%v, want 20/20", e.BalanceDB, e.ImbalanceDB)
	}

	// Right louder -> negative balance, same absolute magnitude. Guards against a
	// sign-flip in BalanceDB = dbL - dbR going unnoticed.
	if e := d.Update(-30, -10, cfg, base); e.BalanceDB != -20 || e.ImbalanceDB != 20 {
		t.Fatalf("right louder: BalanceDB/ImbalanceDB = %v/%v, want -20/20", e.BalanceDB, e.ImbalanceDB)
	}
}

func TestImbalanceDetectorThresholdIsExclusive(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// Exactly at the threshold (12 dB) must not trigger: the condition is strict ">".
	d.Update(-10, -22, cfg, base)
	if e := d.Update(-10, -22, cfg, base.Add(20000*time.Millisecond)); e.JustEntered || e.InImbalance {
		t.Fatalf("triggered at exactly threshold (must be exclusive): %+v", e)
	}
}

func TestImbalanceDetectorBelowPresenceFloorDoesNotTrigger(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// 15 dB apart (over threshold) but the loudest channel is below the presence
	// floor, so this is silence's domain, not imbalance's.
	d.Update(-45, -60, cfg, base)
	if e := d.Update(-45, -60, cfg, base.Add(20000*time.Millisecond)); e.JustEntered || e.InImbalance {
		t.Fatalf("triggered below presence floor: %+v", e)
	}
}

func TestImbalanceDetectorDoubleSilenceNeverTriggers(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	// Both channels silent: ImbalanceDB is 0 and the presence gate is closed.
	d.Update(-60, -60, cfg, base)
	if e := d.Update(-60, -60, cfg, base.Add(20000*time.Millisecond)); e.JustEntered || e.InImbalance {
		t.Fatalf("double silence reported imbalance: %+v", e)
	}
}

// TestImbalanceDetectorPresenceDropEndsCleanly pins the no-hard-reset design:
// when audio drops below the presence floor during a confirmed imbalance (e.g.
// the source dies), the detector must route through the recovery branch and emit
// a clean end event rather than leaving a dangling start.
func TestImbalanceDetectorPresenceDropEndsCleanly(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	d.Update(-10, -30, cfg, base)
	if e := d.Update(-10, -30, cfg, base.Add(15000*time.Millisecond)); !e.JustEntered {
		t.Fatalf("did not enter imbalance: %+v", e)
	}

	// Source dies completely (both channels silent) - presence gate closes.
	if e := d.Update(-60, -60, cfg, base.Add(16000*time.Millisecond)); e.JustRecovered || !e.InImbalance {
		t.Fatalf("ended before recovery window after presence drop: %+v", e)
	}
	e := d.Update(-60, -60, cfg, base.Add(21000*time.Millisecond))
	if !e.JustRecovered {
		t.Fatalf("imbalance did not end after presence dropped away: %+v", e)
	}
}

func TestImbalanceDetectorResetClearsState(t *testing.T) {
	t.Parallel()

	d := NewImbalanceDetector()
	cfg := testImbalanceConfig()
	base := time.Now()

	d.Update(-10, -30, cfg, base)
	d.Update(-10, -30, cfg, base.Add(15000*time.Millisecond)) // confirmed

	d.Reset()

	// After Reset the duration counter restarts: an immediate imbalanced sample
	// must not be treated as already-confirmed.
	if e := d.Update(-10, -30, cfg, base.Add(16000*time.Millisecond)); e.InImbalance || e.JustEntered {
		t.Fatalf("state leaked after Reset: %+v", e)
	}
}
