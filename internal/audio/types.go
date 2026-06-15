package audio

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// ImbalanceLevel represents the channel imbalance detection state.
type ImbalanceLevel string

// ImbalanceLevelActive indicates channel imbalance is confirmed.
const ImbalanceLevelActive ImbalanceLevel = "active"

// AudioLevels is the current audio level measurements for VU meters.
type AudioLevels struct {
	Left              float64      `json:"left"`       // dB
	Right             float64      `json:"right"`      // dB
	PeakLeft          float64      `json:"peak_left"`  // dB
	PeakRight         float64      `json:"peak_right"` // dB
	Silence           bool         `json:"silence,omitzero"`
	SilenceDurationMs int64        `json:"silence_duration_ms,omitzero"`
	SilenceLevel      SilenceLevel `json:"silence_level,omitzero"`

	ChannelImbalance           bool           `json:"channel_imbalance,omitzero"`
	ChannelImbalanceDurationMs int64          `json:"channel_imbalance_duration_ms,omitzero"`
	ChannelImbalanceLevel      ImbalanceLevel `json:"channel_imbalance_level,omitzero"`
	// BalanceDB and ImbalanceDB are continuous meter values like Left/Right, so
	// they are always sent (no omitzero): 0 is a meaningful "perfectly balanced"
	// reading that live meter and API consumers expect, not an absent value.
	BalanceDB   float64 `json:"balance_db"`   // dB; signed L-R
	ImbalanceDB float64 `json:"imbalance_db"` // dB; abs(L-R)

	ClipLeft  int `json:"clip_left,omitzero"`
	ClipRight int `json:"clip_right,omitzero"`
}

// Device represents an available audio input device.
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
