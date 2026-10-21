package types

import "time"

// RecordingStorageStatus is a cached free-space observation, not a writeability guarantee.
type RecordingStorageStatus struct {
	Path           string    `json:"path"`
	AvailableBytes uint64    `json:"available_bytes"`
	TotalBytes     uint64    `json:"total_bytes"` // Linux/macOS: used plus available, leaving out free root-reserved blocks; Windows: the caller's total.
	ThresholdBytes uint64    `json:"threshold_bytes"`
	RecoveryBytes  uint64    `json:"recovery_bytes"` // A low path stays low until this much is free.
	Low            bool      `json:"low"`
	Error          string    `json:"error,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

// Unhealthy reports whether the path is low or could not be checked.
func (s *RecordingStorageStatus) Unhealthy() bool {
	return s.Low || s.Error != ""
}
