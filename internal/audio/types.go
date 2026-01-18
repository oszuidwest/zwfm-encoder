package audio

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// AudioLevels is the current audio level measurements.
type AudioLevels struct {
	// Left is the RMS level in dB for the left channel.
	Left float64 `json:"left"`
	// Right is the RMS level in dB for the right channel.
	Right float64 `json:"right"`
	// PeakLeft is the peak level in dB for the left channel.
	PeakLeft float64 `json:"peak_left"`
	// PeakRight is the peak level in dB for the right channel.
	PeakRight float64 `json:"peak_right"`
	// Silence reports whether audio is below the silence threshold.
	Silence bool `json:"silence,omitzero"`
	// SilenceDurationMs is the silence duration in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms,omitzero"`
	// SilenceLevel indicates the current silence detection state.
	SilenceLevel SilenceLevel `json:"silence_level,omitzero"`
	// ClipLeft is the clipped sample count for the left channel.
	ClipLeft int `json:"clip_left,omitzero"`
	// ClipRight is the clipped sample count for the right channel.
	ClipRight int `json:"clip_right,omitzero"`
}

// Device represents an available audio input device.
type Device struct {
	// ID is the device identifier.
	ID string `json:"id"`
	// Name is the device display name.
	Name string `json:"name"`
}
