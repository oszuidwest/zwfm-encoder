package audio

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// AudioLevels is the current audio level measurements.
type AudioLevels struct {
	Left              float64      `json:"left"`                         // RMS level in dB
	Right             float64      `json:"right"`                        // RMS level in dB
	PeakLeft          float64      `json:"peak_left"`                    // Peak level in dB
	PeakRight         float64      `json:"peak_right"`                   // Peak level in dB
	Silence           bool         `json:"silence,omitzero"`             // True if audio below threshold
	SilenceDurationMs int64        `json:"silence_duration_ms,omitzero"` // Silence duration in milliseconds
	SilenceLevel      SilenceLevel `json:"silence_level,omitzero"`       // "active" when in confirmed silence state
	ClipLeft          int          `json:"clip_left,omitzero"`           // Clipped samples on left channel
	ClipRight         int          `json:"clip_right,omitzero"`          // Clipped samples on right channel
}

// Device represents an available audio input device.
type Device struct {
	ID   string `json:"id"`   // Device identifier
	Name string `json:"name"` // Device display name
}
