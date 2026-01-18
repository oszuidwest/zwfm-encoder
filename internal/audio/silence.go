package audio

import (
	"sync"
	"time"
)

// SilenceConfig holds the configurable thresholds for silence detection.
type SilenceConfig struct {
	// Threshold is the dB level below which audio is considered silent.
	Threshold float64
	// DurationMs is the number of milliseconds of silence before triggering.
	DurationMs int64
	// RecoveryMs is the number of milliseconds of audio before considering recovered.
	RecoveryMs int64
}

// SilenceEvent represents the result of a silence detection update.
type SilenceEvent struct {
	// InSilence reports whether silence is currently confirmed.
	InSilence bool
	// DurationMs is the current silence duration in milliseconds (0 if not silent).
	DurationMs int64
	// Level is the silence level string.
	Level SilenceLevel

	// CurrentLevelL is the current left channel level in dB.
	CurrentLevelL float64
	// CurrentLevelR is the current right channel level in dB.
	CurrentLevelR float64

	// JustEntered is true on the frame when silence is first confirmed.
	JustEntered bool
	// JustRecovered is true on the frame when recovery completes.
	JustRecovered bool
	// TotalDurationMs is the total silence duration in milliseconds.
	TotalDurationMs int64
	// RecoveryDurationMs is how long audio was good before recovery was confirmed.
	RecoveryDurationMs int64
}

// SilenceDetector tracks audio silence state and generates detection events.
// It is safe for concurrent use.
type SilenceDetector struct {
	mu                sync.Mutex
	silenceStart      time.Time // when current silence period started
	recoveryStart     time.Time // when audio returned after silence
	inSilence         bool      // currently in confirmed silence state
	silenceDurationMs int64     // tracks duration in ms for recovery reporting
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

	event := SilenceEvent{
		CurrentLevelL: dbL,
		CurrentLevelR: dbR,
	}

	if audioIsSilent {
		d.recoveryStart = time.Time{}

		if d.silenceStart.IsZero() {
			d.silenceStart = now
		}

		silenceDurationMs := now.Sub(d.silenceStart).Milliseconds()
		d.silenceDurationMs = silenceDurationMs

		if d.inSilence {
			// Already in confirmed silence state
			event.InSilence = true
			event.DurationMs = silenceDurationMs
			event.Level = SilenceLevelActive
		} else if silenceDurationMs >= cfg.DurationMs {
			// Just crossed the duration threshold - enter silence state
			d.inSilence = true
			event.InSilence = true
			event.DurationMs = silenceDurationMs
			event.Level = SilenceLevelActive
			event.JustEntered = true
		}
	} else {
		// Audio is above threshold - preserve silence start during recovery.
		if !d.inSilence {
			d.silenceStart = time.Time{}
		}

		if d.inSilence {
			// Was in silence, now have audio - check recovery
			if d.recoveryStart.IsZero() {
				d.recoveryStart = now
			}

			recoveryDurationMs := now.Sub(d.recoveryStart).Milliseconds()

			if recoveryDurationMs >= cfg.RecoveryMs {
				event.JustRecovered = true
				event.TotalDurationMs = d.silenceDurationMs
				event.RecoveryDurationMs = recoveryDurationMs

				d.inSilence = false
				d.silenceDurationMs = 0
				d.silenceStart = time.Time{}
				d.recoveryStart = time.Time{}
			} else {
				// Still in recovery period - remain in silence state
				event.InSilence = true
				event.Level = SilenceLevelActive
			}
		}
	}

	return event
}

// Reset clears the silence detection state.
func (d *SilenceDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.silenceStart = time.Time{}
	d.recoveryStart = time.Time{}
	d.inSilence = false
	d.silenceDurationMs = 0
}
