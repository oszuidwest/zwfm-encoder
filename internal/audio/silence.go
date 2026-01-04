package audio

import (
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// SilenceConfig holds the configurable thresholds for silence detection.
type SilenceConfig struct {
	Threshold  float64 // dB level below which audio is considered silent
	DurationMs int64   // milliseconds of silence before triggering
	RecoveryMs int64   // milliseconds of audio before considering recovered
}

// SilenceEvent represents the result of a silence detection update.
type SilenceEvent struct {
	// Current state
	InSilence  bool               // Currently in confirmed silence state
	DurationMs int64              // Current silence duration in ms (0 if not silent)
	Level      types.SilenceLevel // "active" when in silence, "" otherwise

	// Current audio levels (for notifications)
	CurrentLevelL float64 // Current left channel level in dB
	CurrentLevelR float64 // Current right channel level in dB

	// State transitions (for triggering notifications)
	JustEntered     bool  // True on the frame when silence is first confirmed
	JustRecovered   bool  // True on the frame when recovery completes
	TotalDurationMs int64 // Total silence duration in ms (only set when JustRecovered)
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
			event.Level = types.SilenceLevelActive
		} else if silenceDurationMs >= cfg.DurationMs {
			// Just crossed the duration threshold - enter silence state
			d.inSilence = true
			event.InSilence = true
			event.DurationMs = silenceDurationMs
			event.Level = types.SilenceLevelActive
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

				d.inSilence = false
				d.silenceDurationMs = 0
				d.silenceStart = time.Time{}
				d.recoveryStart = time.Time{}
			} else {
				// Still in recovery period - remain in silence state
				event.InSilence = true
				event.Level = types.SilenceLevelActive
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
