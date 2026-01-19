package audio

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// AudioLevels is the current audio level measurements for VU meters.
type AudioLevels struct {
	// Left is the left channel RMS level in dB.
	Left float64 `json:"left"`
	// Right is the right channel RMS level in dB.
	Right float64 `json:"right"`
	// PeakLeft is the left channel peak level in dB.
	PeakLeft float64 `json:"peak_left"`
	// PeakRight is the right channel peak level in dB.
	PeakRight float64 `json:"peak_right"`
	// Silence reports whether audio is below the configured silence threshold.
	Silence bool `json:"silence,omitzero"`
	// SilenceDurationMs is how long silence has lasted in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms,omitzero"`
	// SilenceLevel indicates the silence detection state (active or empty).
	SilenceLevel SilenceLevel `json:"silence_level,omitzero"`
	// ClipLeft is how many samples clipped on the left channel this frame.
	ClipLeft int `json:"clip_left,omitzero"`
	// ClipRight is how many samples clipped on the right channel this frame.
	ClipRight int `json:"clip_right,omitzero"`
}

// Device represents an available audio input device.
type Device struct {
	// ID is the device identifier.
	ID string `json:"id"`
	// Name is the device display name.
	Name string `json:"name"`
}
