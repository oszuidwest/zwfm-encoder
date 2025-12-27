// Package types provides shared type definitions used across the encoder.
package types

import "time"

// EncoderState represents the current state of the encoder.
type EncoderState string

const (
	// StateStopped indicates the encoder is not running.
	StateStopped EncoderState = "stopped"
	// StateStarting indicates the encoder is initializing.
	StateStarting EncoderState = "starting"
	// StateRunning indicates the encoder is actively processing audio.
	StateRunning EncoderState = "running"
	// StateStopping indicates the encoder is shutting down.
	StateStopping EncoderState = "stopping"
)

// Retry configuration constants.
const (
	InitialRetryDelay = 3 * time.Second
	MaxRetryDelay     = 60 * time.Second
	MaxRetries        = 10
	SuccessThreshold  = 30 * time.Second // Reset retry count after running this long
	StableThreshold   = 10 * time.Second // Consider connection stable after this duration
)

// Shutdown configuration constants.
const (
	ShutdownTimeout = 3 * time.Second       // Time to wait for graceful shutdown before SIGKILL
	PollInterval    = 50 * time.Millisecond // Interval for polling process state
)

// Output represents a single SRT output destination.
type Output struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	StreamID   string `json:"streamid"`
	Codec      string `json:"codec"`
	MaxRetries int    `json:"max_retries,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// DefaultMaxRetries is the default number of retry attempts for outputs.
const DefaultMaxRetries = 99

// MaxRetriesOrDefault returns the configured max retries or the default value.
func (o *Output) MaxRetriesOrDefault() int {
	if o.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return o.MaxRetries
}

// CodecPreset defines FFmpeg encoding parameters for a codec.
type CodecPreset struct {
	Args   []string
	Format string
}

// CodecPresets maps codec names to their FFmpeg configuration.
var CodecPresets = map[string]CodecPreset{
	"mp2": {[]string{"libtwolame", "-b:a", "384k", "-psymodel", "4"}, "mp2"},
	"mp3": {[]string{"libmp3lame", "-b:a", "320k"}, "mp3"},
	"ogg": {[]string{"libvorbis", "-qscale:a", "10"}, "ogg"},
	"wav": {[]string{"pcm_s16le"}, "matroska"},
}

// DefaultCodec is used when an unknown codec is specified.
const DefaultCodec = "wav"

// CodecArgs returns FFmpeg codec arguments for this output's codec.
func (o *Output) CodecArgs() []string {
	if preset, ok := CodecPresets[o.Codec]; ok {
		return preset.Args
	}
	return CodecPresets[DefaultCodec].Args
}

// Format returns the FFmpeg output format for this output's codec.
func (o *Output) Format() string {
	if preset, ok := CodecPresets[o.Codec]; ok {
		return preset.Format
	}
	return CodecPresets[DefaultCodec].Format
}

// OutputStatus contains runtime status for an output.
type OutputStatus struct {
	Running    bool   `json:"running"`
	Stable     bool   `json:"stable,omitzero"`
	LastError  string `json:"last_error,omitzero"`
	RetryCount int    `json:"retry_count,omitzero"`
	MaxRetries int    `json:"max_retries"`
	GivenUp    bool   `json:"given_up,omitzero"`
}

// EncoderStatus contains a summary of the encoder's current operational state.
type EncoderStatus struct {
	State            EncoderState `json:"state"`
	Uptime           string       `json:"uptime,omitzero"`
	LastError        string       `json:"last_error,omitzero"`
	OutputCount      int          `json:"output_count"`
	SourceRetryCount int          `json:"source_retry_count,omitzero"`
	SourceMaxRetries int          `json:"source_max_retries"`
}

// SilenceLevel represents the silence detection state.
type SilenceLevel string

const (
	SilenceLevelNone   SilenceLevel = ""       // No silence detected
	SilenceLevelActive SilenceLevel = "active" // Silence confirmed (duration threshold exceeded)
)

// AudioLevels contains current audio level measurements.
type AudioLevels struct {
	Left            float64      `json:"left"`                      // RMS level in dB
	Right           float64      `json:"right"`                     // RMS level in dB
	PeakLeft        float64      `json:"peak_left"`                 // Peak level in dB
	PeakRight       float64      `json:"peak_right"`                // Peak level in dB
	Silence         bool         `json:"silence,omitzero"`          // True if audio below threshold
	SilenceDuration float64      `json:"silence_duration,omitzero"` // Silence duration in seconds
	SilenceLevel    SilenceLevel `json:"silence_level,omitzero"`    // "active" when in confirmed silence state
	ClipLeft        int          `json:"clip_left,omitzero"`        // Clipped samples on left channel
	ClipRight       int          `json:"clip_right,omitzero"`       // Clipped samples on right channel
}

// AudioMetrics contains audio level metrics for callback processing.
type AudioMetrics struct {
	RMSL, RMSR      float64      // RMS levels in dB
	PeakL, PeakR    float64      // Peak levels in dB
	Silence         bool         // True if audio below threshold
	SilenceDuration float64      // Silence duration in seconds
	SilenceLevel    SilenceLevel // "active" when in confirmed silence state
	ClipL, ClipR    int          // Clipped sample counts
}

// WSStatusResponse is sent to clients with full encoder and output status.
type WSStatusResponse struct {
	Type             string                  `json:"type"`
	Encoder          EncoderStatus           `json:"encoder"`
	Outputs          []Output                `json:"outputs"`
	OutputStatus     map[string]OutputStatus `json:"output_status"`
	Devices          []AudioDevice           `json:"devices"`
	SilenceThreshold float64                 `json:"silence_threshold"`
	SilenceDuration  float64                 `json:"silence_duration"`
	SilenceRecovery  float64                 `json:"silence_recovery"`
	SilenceWebhook   string                  `json:"silence_webhook"`
	SilenceLogPath   string                  `json:"silence_log_path"`
	EmailSMTPHost    string                  `json:"email_smtp_host"`
	EmailSMTPPort    int                     `json:"email_smtp_port"`
	EmailFromName    string                  `json:"email_from_name"`
	EmailUsername    string                  `json:"email_username"`
	EmailRecipients  string                  `json:"email_recipients"`
	Settings         WSSettings              `json:"settings"`
	Version          VersionInfo             `json:"version"`
}

// WSSettings contains the settings sub-object in status responses.
type WSSettings struct {
	AudioInput string `json:"audio_input"`
	Platform   string `json:"platform"`
}

// WSLevelsResponse is sent to clients with audio level updates.
type WSLevelsResponse struct {
	Type   string      `json:"type"`
	Levels AudioLevels `json:"levels"`
}

// WSTestResult is sent to clients after a test operation completes.
type WSTestResult struct {
	Type     string `json:"type"`
	TestType string `json:"test_type"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

// WSSilenceLogResult is sent to clients with silence log entries.
type WSSilenceLogResult struct {
	Type    string            `json:"type"`
	Success bool              `json:"success"`
	Error   string            `json:"error,omitempty"`
	Entries []SilenceLogEntry `json:"entries,omitempty"`
	Path    string            `json:"path,omitempty"`
}

// SilenceLogEntry represents a single entry in the silence log.
type SilenceLogEntry struct {
	Timestamp   string  `json:"timestamp"`
	Event       string  `json:"event"`
	DurationSec float64 `json:"duration_sec,omitempty"` // Duration of the silence period in seconds.
	ThresholdDB float64 `json:"threshold_db"`
}

// AudioDevice represents an available audio input device.
type AudioDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EmailConfig contains SMTP server settings for email notifications.
type EmailConfig struct {
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	FromName   string `json:"from_name,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Recipients string `json:"recipients,omitempty"`
}

// VersionInfo contains version comparison data.
type VersionInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	UpdateAvail bool   `json:"update_available"`
	Commit      string `json:"commit,omitempty"`
	BuildTime   string `json:"build_time,omitempty"`
}
