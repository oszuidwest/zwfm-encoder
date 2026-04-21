package audio

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// SilentChannels identifies which stereo channel(s) triggered silence detection.
type SilentChannels string

const (
	// SilentChannelsBoth indicates both L and R channels are below the silence threshold.
	SilentChannelsBoth SilentChannels = "both"
	// SilentChannelsLeft indicates only the left channel is below the silence threshold.
	SilentChannelsLeft SilentChannels = "left"
	// SilentChannelsRight indicates only the right channel is below the silence threshold.
	SilentChannelsRight SilentChannels = "right"
)

// AudioLevels is the current audio level measurements for VU meters.
type AudioLevels struct {
	Left              float64        `json:"left"`       // dB
	Right             float64        `json:"right"`      // dB
	PeakLeft          float64        `json:"peak_left"`  // dB
	PeakRight         float64        `json:"peak_right"` // dB
	Silence           bool           `json:"silence,omitzero"`
	SilenceDurationMs int64          `json:"silence_duration_ms,omitzero"`
	SilenceLevel      SilenceLevel   `json:"silence_level,omitzero"`
	SilentChannels    SilentChannels `json:"silent_channels,omitzero"`
	ClipLeft          int            `json:"clip_left,omitzero"`
	ClipRight         int            `json:"clip_right,omitzero"`
}

// Device represents an available audio input device.
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
