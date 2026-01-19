package audio

import (
	"sync"
	"time"
)

// SilenceConfig holds the configurable thresholds for silence detection.
type SilenceConfig struct {
	// Threshold is the audio level in dB below which silence is detected.
	Threshold float64
	// DurationMs is how long audio must be below threshold before alerting.
	DurationMs int64
	// RecoveryMs is how long audio must be above threshold before clearing the alert.
	RecoveryMs int64
}

// SilenceEvent represents the result of a silence detection update.
type SilenceEvent struct {
	// InSilence reports whether silence is currently confirmed.
	InSilence bool
	// DurationMs is how long silence has lasted in milliseconds (0 if not silent).
	DurationMs int64
	// Level indicates the silence detection state (active or empty).
	Level SilenceLevel

	// CurrentLevelL is the left channel audio level in dB at detection time.
	CurrentLevelL float64
	// CurrentLevelR is the right channel audio level in dB at detection time.
	CurrentLevelR float64

	// JustEntered reports whether silence was just confirmed on this update.
	JustEntered bool
	// JustRecovered reports whether recovery just completed on this update.
	JustRecovered bool
	// TotalDurationMs is the total silence duration in milliseconds when recovery completes.
	TotalDurationMs int64
	// RecoveryDurationMs is how long audio was above threshold before recovery was confirmed.
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
