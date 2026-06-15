package audio

import (
	"math"
	"sync"
	"time"
)

// ImbalanceConfig controls channel imbalance detection thresholds.
type ImbalanceConfig struct {
	ThresholdDB float64 // dB; strict abs(L-R) trigger threshold
	DurationMs  int64   // milliseconds above threshold before entering active state
	RecoveryMs  int64   // milliseconds below threshold before recovery
	// PresenceFloorDB is the minimum channel level required to evaluate imbalance.
	PresenceFloorDB float64
}

// ImbalanceEvent reports channel-balance state for one detector update.
type ImbalanceEvent struct {
	InImbalance bool           // active, including the recovery window
	DurationMs  int64          // active duration, or 0 during recovery
	Level       ImbalanceLevel // active label for API and websocket payloads

	BalanceDB     float64 // dB; signed L-R, positive means left louder
	ImbalanceDB   float64 // dB; abs(L-R)
	CurrentLevelL float64 // dB
	CurrentLevelR float64 // dB

	JustEntered        bool  // true only on entry
	JustRecovered      bool  // true only on recovery
	TotalDurationMs    int64 // final active duration on recovery
	RecoveryDurationMs int64 // elapsed recovery time on recovery
}

// ImbalanceDetector detects sustained L/R level differences.
// It is safe for concurrent use.
type ImbalanceDetector struct {
	mu       sync.Mutex
	debounce debouncer
}

// NewImbalanceDetector creates a new channel imbalance detector.
func NewImbalanceDetector() *ImbalanceDetector {
	return &ImbalanceDetector{}
}

// Update advances imbalance detection for one L/R level sample.
func (d *ImbalanceDetector) Update(dbL, dbR float64, cfg ImbalanceConfig, now time.Time) ImbalanceEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	balanceDB := dbL - dbR
	imbalanceDB := math.Abs(balanceDB)
	// Presence gate keeps double-silence in the silence detector's domain.
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
