package audio

import (
	"testing"
	"time"
)

func testSilenceConfig() SilenceConfig {
	return SilenceConfig{Threshold: -40, DurationMs: 15000, RecoveryMs: 5000}
}
func TestSilenceDetectorEntersAfterDuration(t *testing.T) {
	t.Parallel()
	d := NewSilenceDetector()
	cfg := testSilenceConfig()
	base := time.Now()
	if e := d.Update(-50, -55, cfg, base); e.JustEntered || e.InSilence {
		t.Fatalf("entered too early: %+v", e)
	}
	e := d.Update(-50, -55, cfg, base.Add(15000*time.Millisecond))
	if !e.JustEntered || !e.InSilence || e.Level != SilenceLevelActive {
		t.Fatalf("did not enter at duration threshold: %+v", e)
	}
	if e.DurationMs != 15000 {
		t.Fatalf("DurationMs = %d, want 15000", e.DurationMs)
	}
}
func TestSilenceDetectorRequiresBothChannelsSilent(t *testing.T) {
	t.Parallel()
	d := NewSilenceDetector()
	cfg := testSilenceConfig()
	base := time.Now()
	d.Update(-50, -10, cfg, base)
	if e := d.Update(-50, -10, cfg, base.Add(20000*time.Millisecond)); e.JustEntered || e.InSilence {
		t.Fatalf("entered silence with one channel loud: %+v", e)
	}
}
func TestSilenceDetectorRecoversAfterRecovery(t *testing.T) {
	t.Parallel()
	d := NewSilenceDetector()
	cfg := testSilenceConfig()
	base := time.Now()
	d.Update(-50, -55, cfg, base)
	d.Update(-50, -55, cfg, base.Add(15000*time.Millisecond)) // confirmed
	if e := d.Update(-10, -12, cfg, base.Add(16000*time.Millisecond)); e.JustRecovered || !e.InSilence {
		t.Fatalf("recovered too early: %+v", e)
	}
	e := d.Update(-10, -12, cfg, base.Add(21000*time.Millisecond))
	if !e.JustRecovered {
		t.Fatalf("did not recover after recovery window: %+v", e)
	}
	if e.TotalDurationMs == 0 {
		t.Fatalf("TotalDurationMs = 0, want the confirmed silence duration")
	}
}
func TestSilenceDetectorResetClearsState(t *testing.T) {
	t.Parallel()
	d := NewSilenceDetector()
	cfg := testSilenceConfig()
	base := time.Now()
	d.Update(-50, -55, cfg, base)
	d.Update(-50, -55, cfg, base.Add(15000*time.Millisecond)) // confirmed
	d.Reset()
	if e := d.Update(-50, -55, cfg, base.Add(16000*time.Millisecond)); e.InSilence || e.JustEntered {
		t.Fatalf("state leaked after Reset: %+v", e)
	}
}
