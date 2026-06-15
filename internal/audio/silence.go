package audio

import (
	"sync"
	"time"
)

// SilenceConfig holds the configurable thresholds for silence detection.
type SilenceConfig struct {
	Threshold  float64 // dB
	DurationMs int64
	RecoveryMs int64
}

// SilenceEvent represents the result of a silence detection update.
type SilenceEvent struct {
	InSilence  bool
	DurationMs int64
	Level      SilenceLevel

	CurrentLevelL float64 // dB
	CurrentLevelR float64 // dB

	JustEntered        bool
	JustRecovered      bool
	TotalDurationMs    int64
	RecoveryDurationMs int64
}

// SilenceDetector tracks audio silence state and generates detection events.
// It is safe for concurrent use.
type SilenceDetector struct {
	mu       sync.Mutex
	debounce debouncer
}

// NewSilenceDetector creates a new silence detector.
func NewSilenceDetector() *SilenceDetector {
	return &SilenceDetector{}
}

// Update updates the silence detection state with new audio levels and returns the current state.
func (d *SilenceDetector) Update(dbL, dbR float64, cfg SilenceConfig, now time.Time) SilenceEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	audioIsSilent := dbL < cfg.Threshold && dbR < cfg.Threshold
	r := d.debounce.update(audioIsSilent, cfg.DurationMs, cfg.RecoveryMs, now)

	event := SilenceEvent{
		InSilence:          r.active,
		DurationMs:         r.durationMs,
		CurrentLevelL:      dbL,
		CurrentLevelR:      dbR,
		JustEntered:        r.justEntered,
		JustRecovered:      r.justRecovered,
		TotalDurationMs:    r.totalDurationMs,
		RecoveryDurationMs: r.recoveryDurationMs,
	}
	if r.active {
		event.Level = SilenceLevelActive
	}
	return event
}

// Reset clears the silence detection state.
func (d *SilenceDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.debounce.reset()
}
