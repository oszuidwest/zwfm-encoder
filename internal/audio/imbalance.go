package audio

import (
	"math"
	"sync"
	"time"
)

// ImbalanceConfig holds the configurable thresholds for channel imbalance detection.
type ImbalanceConfig struct {
	ThresholdDB     float64 // dB; alarm when abs(L-R) exceeds this
	DurationMs      int64
	RecoveryMs      int64
	PresenceFloorDB float64 // dB; imbalance is evaluated only when max(L,R) >= this (reuses the silence threshold)
}

// ImbalanceEvent represents the result of a channel imbalance detection update.
type ImbalanceEvent struct {
	InImbalance bool
	DurationMs  int64
	Level       ImbalanceLevel

	BalanceDB     float64 // dB; signed L-R (positive = left louder)
	ImbalanceDB   float64 // dB; abs(L-R), the alarm magnitude
	CurrentLevelL float64 // dB
	CurrentLevelR float64 // dB

	JustEntered        bool
	JustRecovered      bool
	TotalDurationMs    int64
	RecoveryDurationMs int64
}

// ImbalanceDetector tracks L/R channel imbalance state and generates detection events.
// It is safe for concurrent use. The state machine mirrors [SilenceDetector]: a confirmed
// state is entered after DurationMs above threshold and cleared after RecoveryMs below it.
type ImbalanceDetector struct {
	mu                  sync.Mutex
	imbalanceStart      time.Time // when current imbalance period started
	recoveryStart       time.Time // when balance returned after imbalance
	inImbalance         bool      // currently in confirmed imbalance state
	imbalanceDurationMs int64     // tracks duration in ms for recovery reporting
}

// NewImbalanceDetector creates a new channel imbalance detector.
func NewImbalanceDetector() *ImbalanceDetector {
	return &ImbalanceDetector{}
}

// Update updates the imbalance detection state with new audio levels and returns the current state.
func (d *ImbalanceDetector) Update(dbL, dbR float64, cfg ImbalanceConfig, now time.Time) ImbalanceEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	balanceDB := dbL - dbR
	imbalanceDB := math.Abs(balanceDB)
	// present is the exact logical complement of "both channels silent", so the
	// instantaneous silence and imbalance conditions are mutually exclusive.
	present := max(dbL, dbR) >= cfg.PresenceFloorDB
	channelsImbalanced := present && imbalanceDB > cfg.ThresholdDB

	event := ImbalanceEvent{
		BalanceDB:     balanceDB,
		ImbalanceDB:   imbalanceDB,
		CurrentLevelL: dbL,
		CurrentLevelR: dbR,
	}

	if channelsImbalanced {
		d.recoveryStart = time.Time{}

		if d.imbalanceStart.IsZero() {
			d.imbalanceStart = now
		}

		imbalanceDurationMs := now.Sub(d.imbalanceStart).Milliseconds()
		d.imbalanceDurationMs = imbalanceDurationMs

		if d.inImbalance {
			// Already in confirmed imbalance state
			event.InImbalance = true
			event.DurationMs = imbalanceDurationMs
			event.Level = ImbalanceLevelActive
		} else if imbalanceDurationMs >= cfg.DurationMs {
			// Just crossed the duration threshold - enter imbalance state
			d.inImbalance = true
			event.InImbalance = true
			event.DurationMs = imbalanceDurationMs
			event.Level = ImbalanceLevelActive
			event.JustEntered = true
		}
	} else {
		// Channels are balanced (or below the presence floor) - preserve imbalance start during recovery.
		if !d.inImbalance {
			d.imbalanceStart = time.Time{}
		}

		if d.inImbalance {
			// Was imbalanced, now balanced - check recovery
			if d.recoveryStart.IsZero() {
				d.recoveryStart = now
			}

			recoveryDurationMs := now.Sub(d.recoveryStart).Milliseconds()

			if recoveryDurationMs >= cfg.RecoveryMs {
				event.JustRecovered = true
				event.TotalDurationMs = d.imbalanceDurationMs
				event.RecoveryDurationMs = recoveryDurationMs

				d.inImbalance = false
				d.imbalanceDurationMs = 0
				d.imbalanceStart = time.Time{}
				d.recoveryStart = time.Time{}
			} else {
				// Still in recovery period - remain in imbalance state
				event.InImbalance = true
				event.Level = ImbalanceLevelActive
			}
		}
	}

	return event
}

// Reset clears the imbalance detection state.
func (d *ImbalanceDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.imbalanceStart = time.Time{}
	d.recoveryStart = time.Time{}
	d.inImbalance = false
	d.imbalanceDurationMs = 0
}
