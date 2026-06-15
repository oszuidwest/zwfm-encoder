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
// It is safe for concurrent use. The timing/hysteresis is the shared [debouncer]; only
// the trigger predicate differs from [SilenceDetector]: present && abs(L-R) > threshold.
type ImbalanceDetector struct {
	mu       sync.Mutex
	debounce debouncer
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

	r := d.debounce.update(channelsImbalanced, cfg.DurationMs, cfg.RecoveryMs, now)

	event := ImbalanceEvent{
		InImbalance:        r.active,
		DurationMs:         r.durationMs,
		BalanceDB:          balanceDB,
		ImbalanceDB:        imbalanceDB,
		CurrentLevelL:      dbL,
		CurrentLevelR:      dbR,
		JustEntered:        r.justEntered,
		JustRecovered:      r.justRecovered,
		TotalDurationMs:    r.totalDurationMs,
		RecoveryDurationMs: r.recoveryDurationMs,
	}
	if r.active {
		event.Level = ImbalanceLevelActive
	}
	return event
}

// Reset clears the imbalance detection state.
func (d *ImbalanceDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.debounce.reset()
}
