package audio

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// SilenceConfig holds the configurable thresholds for silence detection.
type SilenceConfig struct {
	Threshold float64 // dB level below which audio is considered silent
	Duration  float64 // seconds of silence before triggering
	Recovery  float64 // seconds of audio before considering recovered
}

// SilenceEvent represents the result of a silence detection update.
type SilenceEvent struct {
	// Current state
	InSilence bool               // Currently in confirmed silence state
	Duration  float64            // Current silence duration (0 if not silent)
	Level     types.SilenceLevel // "active" when in silence, "" otherwise

	// State transitions (for triggering notifications)
	JustEntered   bool    // True on the frame when silence is first confirmed
	JustRecovered bool    // True on the frame when recovery completes
	TotalDuration float64 // Total silence duration (only set when JustRecovered)
}

// SilenceDetector tracks audio silence state and generates detection events.
type SilenceDetector struct {
	silenceStart    time.Time // when current silence period started
	recoveryStart   time.Time // when audio returned after silence
	inSilence       bool      // currently in confirmed silence state
	silenceDuration float64   // tracks duration for recovery reporting
}

// NewSilenceDetector creates a new silence detector.
func NewSilenceDetector() *SilenceDetector {
	return &SilenceDetector{}
}

// Update checks audio levels and returns a SilenceEvent describing what happened.
func (d *SilenceDetector) Update(dbL, dbR float64, cfg SilenceConfig, now time.Time) SilenceEvent {
	audioIsSilent := dbL < cfg.Threshold && dbR < cfg.Threshold

	event := SilenceEvent{}

	if audioIsSilent {
		d.recoveryStart = time.Time{}

		if d.silenceStart.IsZero() {
			d.silenceStart = now
		}

		silenceDuration := now.Sub(d.silenceStart).Seconds()
		d.silenceDuration = silenceDuration

		if d.inSilence {
			// Already in confirmed silence state
			event.InSilence = true
			event.Duration = silenceDuration
			event.Level = types.SilenceLevelActive
		} else if silenceDuration >= cfg.Duration {
			// Just crossed the duration threshold - enter silence state
			d.inSilence = true
			event.InSilence = true
			event.Duration = silenceDuration
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

			recoveryDuration := now.Sub(d.recoveryStart).Seconds()

			if recoveryDuration >= cfg.Recovery {
				event.JustRecovered = true
				event.TotalDuration = d.silenceDuration

				d.inSilence = false
				d.silenceDuration = 0
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
	d.silenceStart = time.Time{}
	d.recoveryStart = time.Time{}
	d.inSilence = false
	d.silenceDuration = 0
}
