package types

import "time"

// RecordingStorageStatus is a cached free-space observation, not a writeability guarantee.
type RecordingStorageStatus struct {
	Path           string    `json:"path"`
	AvailableBytes uint64    `json:"available_bytes"`
	TotalBytes     uint64    `json:"total_bytes"`
	ThresholdBytes uint64    `json:"threshold_bytes"`
	Low            bool      `json:"low"`
	Error          string    `json:"error,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

// Unhealthy reports whether the path is below its threshold or could not be checked.
func (s *RecordingStorageStatus) Unhealthy() bool {
	return s.Low || s.Error != ""
}
