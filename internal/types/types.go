// Package types provides shared type definitions used across the encoder.
package types

import (
	"time"
)

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

// ProcessState represents the state of any managed process (output or recorder).
type ProcessState string

const (
	// ProcessStopped indicates the process is not running.
	ProcessStopped ProcessState = "stopped"
	// ProcessDisabled indicates the process is explicitly disabled.
	ProcessDisabled ProcessState = "disabled"
	// ProcessStarting indicates the process is initializing.
	ProcessStarting ProcessState = "starting"
	// ProcessRunning indicates the process is active.
	ProcessRunning ProcessState = "running"
	// ProcessRotating indicates file rotation is in progress (recorders only).
	ProcessRotating ProcessState = "rotating"
	// ProcessStopping indicates the process is shutting down.
	ProcessStopping ProcessState = "stopping"
	// ProcessError indicates the process failed.
	ProcessError ProcessState = "error"
)

// ProcessStatus contains runtime status for any managed process (output or recorder).
type ProcessStatus struct {
	State      ProcessState `json:"state"`                 // Current process state
	Stable     bool         `json:"stable,omitempty"`      // Outputs: running ≥10s
	Exhausted  bool         `json:"exhausted,omitempty"`   // Outputs: max retries reached
	RetryCount int          `json:"retry_count,omitempty"` // Outputs: current retry attempt
	MaxRetries int          `json:"max_retries,omitempty"` // Outputs: max allowed retries
	Error      string       `json:"error,omitempty"`       // Error message
}

const (
	// InitialRetryDelay is the starting delay between retry attempts.
	InitialRetryDelay = 3000 * time.Millisecond
	// MaxRetryDelay is the maximum delay between retry attempts.
	MaxRetryDelay = 60000 * time.Millisecond
	// MaxRetries is the maximum number of retry attempts for the audio source.
	MaxRetries = 10
	// SuccessThreshold is the duration after which retry count resets.
	SuccessThreshold = 30000 * time.Millisecond
	// StableThreshold is the duration after which a connection is considered stable.
	StableThreshold = 10000 * time.Millisecond
)

const (
	// ShutdownTimeout is the duration to wait for graceful shutdown.
	ShutdownTimeout = 3000 * time.Millisecond
	// PollInterval is the interval for polling process state.
	PollInterval = 50 * time.Millisecond
)

// Audio format constants for PCM capture and encoding.
const (
	// SampleRate is the audio sample rate in Hz.
	SampleRate = 48000
	// Channels is the number of audio channels (stereo).
	Channels = 2
)

// Codec represents an audio codec type.
type Codec string

// Supported audio codecs.
const (
	CodecWAV Codec = "wav" // Uncompressed PCM in Matroska container
	CodecMP3 Codec = "mp3" // MPEG Audio Layer III
	CodecMP2 Codec = "mp2" // MPEG Audio Layer II
	CodecOGG Codec = "ogg" // Ogg Vorbis
)

// Output represents a single SRT output destination.
type Output struct {
	ID         string `json:"id"`          // Unique identifier
	Enabled    bool   `json:"enabled"`     // Whether output is active
	Host       string `json:"host"`        // SRT server hostname
	Port       int    `json:"port"`        // SRT server port
	Password   string `json:"password"`    // SRT encryption passphrase
	StreamID   string `json:"stream_id"`   // SRT stream identifier
	Codec      Codec  `json:"codec"`       // Audio codec (mp2, mp3, ogg, wav)
	MaxRetries int    `json:"max_retries"` // Maximum retry attempts (0 = no retries)
	CreatedAt  int64  `json:"created_at"`  // Unix timestamp of creation
}

// IsEnabled reports whether the output is enabled.
func (o *Output) IsEnabled() bool {
	return o.Enabled
}

// DefaultMaxRetries is the default number of retry attempts for outputs.
const DefaultMaxRetries = 99

// OutputRestartDelay is the delay between stopping and starting an output during restart.
const OutputRestartDelay = 2000 * time.Millisecond

// MaxRetriesOrDefault returns the configured max retries or the default value.
func (o *Output) MaxRetriesOrDefault() int {
	if o.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return o.MaxRetries
}

// CodecPreset defines FFmpeg encoding parameters for a codec.
type CodecPreset struct {
	Args   []string // FFmpeg codec arguments
	Format string   // FFmpeg output format
}

// CodecPresets maps codec types to their FFmpeg configuration.
var CodecPresets = map[Codec]CodecPreset{
	CodecMP2: {[]string{"libtwolame", "-b:a", "384k", "-psymodel", "4"}, "mp2"},
	CodecMP3: {[]string{"libmp3lame", "-b:a", "320k"}, "mp3"},
	CodecOGG: {[]string{"libvorbis", "-qscale:a", "10"}, "ogg"},
	CodecWAV: {[]string{"pcm_s16le"}, "matroska"},
}

// CodecArgsFor returns FFmpeg codec arguments for the given codec.
func CodecArgsFor(codec Codec) []string {
	if preset, ok := CodecPresets[codec]; ok {
		return preset.Args
	}
	return CodecPresets[CodecWAV].Args
}

// FormatFor returns the FFmpeg output format for the given codec.
func FormatFor(codec Codec) string {
	if preset, ok := CodecPresets[codec]; ok {
		return preset.Format
	}
	return CodecPresets[CodecWAV].Format
}

// CodecArgs returns FFmpeg codec arguments for this output's codec.
func (o *Output) CodecArgs() []string {
	return CodecArgsFor(o.Codec)
}

// Format returns the FFmpeg output format for this output's codec.
func (o *Output) Format() string {
	return FormatFor(o.Codec)
}

// RotationMode determines how recordings are split into files.
type RotationMode string

// Supported rotation modes.
const (
	RotationHourly RotationMode = "hourly" // Rotate at system clock hour boundaries
)

// StorageMode determines where recordings are saved.
type StorageMode string

// Supported storage modes.
const (
	StorageLocal StorageMode = "local" // Save only to local filesystem
	StorageS3    StorageMode = "s3"    // Upload only to S3
	StorageBoth  StorageMode = "both"  // Save locally AND upload to S3
)

// DefaultRetentionDays is the default number of days to keep recordings.
const DefaultRetentionDays = 90

// DefaultSilenceDumpRetentionDays is the default number of days to keep silence dumps.
const DefaultSilenceDumpRetentionDays = 7

// SilenceDumpConfig contains configuration for silence dump capture.
type SilenceDumpConfig struct {
	Enabled       bool `json:"enabled"`        // Whether dump capture is active
	RetentionDays int  `json:"retention_days"` // Days to keep dump files (default 7)
}

// Recorder represents a recording destination configuration.
type Recorder struct {
	ID           string       `json:"id"`            // Unique identifier
	Name         string       `json:"name"`          // Display name
	Enabled      bool         `json:"enabled"`       // Whether recorder is active
	Codec        Codec        `json:"codec"`         // Audio codec (mp2, mp3, ogg, wav)
	RotationMode RotationMode `json:"rotation_mode"` // hourly or ondemand
	StorageMode  StorageMode  `json:"storage_mode"`  // local, s3, or both
	LocalPath    string       `json:"local_path"`    // Local directory for recordings (required for local/both)

	// S3 configuration (required for s3/both modes)
	S3Endpoint        string `json:"s3_endpoint"`          // S3-compatible endpoint URL
	S3Bucket          string `json:"s3_bucket"`            // S3 bucket name
	S3AccessKeyID     string `json:"s3_access_key_id"`     // S3 access key ID
	S3SecretAccessKey string `json:"s3_secret_access_key"` // S3 secret access key
	// S3 prefix auto-generated: recordings/{sanitized-name}/

	RetentionDays int   `json:"retention_days"` // Days to keep recordings (default 90)
	CreatedAt     int64 `json:"created_at"`     // Unix timestamp of creation
}

// IsEnabled reports whether the recorder is enabled.
func (r *Recorder) IsEnabled() bool {
	return r.Enabled
}

// CodecArgs returns FFmpeg codec arguments for this recorder's codec.
func (r *Recorder) CodecArgs() []string {
	return CodecArgsFor(r.Codec)
}

// Format returns the FFmpeg output format for this recorder's codec.
func (r *Recorder) Format() string {
	return FormatFor(r.Codec)
}

// EncoderStatus contains a summary of the encoder's current operational state.
type EncoderStatus struct {
	State            EncoderState `json:"state"`                       // Current encoder state
	Uptime           string       `json:"uptime,omitzero"`             // Time since start
	LastError        string       `json:"last_error,omitzero"`         // Most recent error
	OutputCount      int          `json:"output_count"`                // Number of outputs
	SourceRetryCount int          `json:"source_retry_count,omitzero"` // Source retry attempts
	SourceMaxRetries int          `json:"source_max_retries"`          // Max source retries
}

// SilenceLevel represents the silence detection state.
type SilenceLevel string

// SilenceLevelActive indicates silence is confirmed.
const SilenceLevelActive SilenceLevel = "active"

// AudioLevels contains current audio level measurements.
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

// AudioMetrics contains audio level metrics for callback processing.
type AudioMetrics struct {
	RMSLeft, RMSRight   float64      // RMS levels in dB
	PeakLeft, PeakRight float64      // Peak levels in dB
	Silence             bool         // True if audio below threshold
	SilenceDurationMs   int64        // Silence duration in milliseconds
	SilenceLevel        SilenceLevel // "active" when in confirmed silence state
	ClipLeft, ClipRight int          // Clipped sample counts
}

// WSStatusResponse is sent to clients with full encoder and output status.
type WSStatusResponse struct {
	Type                string                   `json:"type"`                            // Message type identifier
	FFmpegAvailable     bool                     `json:"ffmpeg_available"`                // FFmpeg binary is available
	Encoder             EncoderStatus            `json:"encoder"`                         // Encoder status
	Outputs             []Output                 `json:"outputs"`                         // Output configurations
	OutputStatus        map[string]ProcessStatus `json:"output_status"`                   // Runtime output status
	Recorders           []Recorder               `json:"recorders"`                       // Recorder configurations
	RecorderStatuses    map[string]ProcessStatus `json:"recorder_statuses"`               // Runtime recorder status
	RecordingAPIKey     string                   `json:"recording_api_key"`               // API key for recording control
	Devices             []AudioDevice            `json:"devices"`                         // Available audio devices
	SilenceThreshold    float64                  `json:"silence_threshold"`               // Silence threshold in dB
	SilenceDurationMs   int64                    `json:"silence_duration_ms"`             // Silence duration in milliseconds
	SilenceRecoveryMs   int64                    `json:"silence_recovery_ms"`             // Recovery duration in milliseconds
	SilenceWebhook      string                   `json:"silence_webhook"`                 // Webhook URL for alerts
	SilenceLogPath      string                   `json:"silence_log_path"`                // Log file path
	SilenceZabbixServer string                   `json:"silence_zabbix_server,omitempty"` // Zabbix server address
	SilenceZabbixPort   int                      `json:"silence_zabbix_port,omitempty"`   // Zabbix server port
	SilenceZabbixHost   string                   `json:"silence_zabbix_host,omitempty"`   // Zabbix host name
	SilenceZabbixKey    string                   `json:"silence_zabbix_key,omitempty"`    // Zabbix item key
	GraphTenantID       string                   `json:"graph_tenant_id"`                 // Azure AD tenant ID
	GraphClientID       string                   `json:"graph_client_id"`                 // App registration client ID
	GraphFromAddress    string                   `json:"graph_from_address"`              // Shared mailbox address
	GraphRecipients     string                   `json:"graph_recipients"`                // Comma-separated recipients
	GraphSecretExpiry   SecretExpiryInfo         `json:"graph_secret_expiry"`             // Client secret expiration info
	SilenceDump         SilenceDumpConfig        `json:"silence_dump"`                    // Silence dump configuration
	Settings            WSSettings               `json:"settings"`                        // Current settings
	Version             VersionInfo              `json:"version"`                         // Version information
}

// WSSettings contains the settings sub-object in status responses.
type WSSettings struct {
	AudioInput string `json:"audio_input"` // Selected audio input device
	Platform   string `json:"platform"`    // Operating system platform
}

// WSLevelsResponse is sent to clients with audio level updates.
type WSLevelsResponse struct {
	Type   string      `json:"type"`   // Message type identifier
	Levels AudioLevels `json:"levels"` // Current audio levels
}

// WSTestResult is sent to clients after a test operation completes.
type WSTestResult struct {
	Type     string `json:"type"`            // Message type identifier
	TestType string `json:"test_type"`       // Type of test performed
	Success  bool   `json:"success"`         // Test succeeded
	Error    string `json:"error,omitempty"` // Error message if failed
}

// WSSilenceLogResult is sent to clients with silence log entries.
type WSSilenceLogResult struct {
	Type    string            `json:"type"`              // Message type identifier
	Success bool              `json:"success"`           // Operation succeeded
	Error   string            `json:"error,omitempty"`   // Error message if failed
	Entries []SilenceLogEntry `json:"entries,omitempty"` // Log entries
	Path    string            `json:"path,omitempty"`    // Log file path
}

// SilenceLogEntry represents a single entry in the silence log.
type SilenceLogEntry struct {
	Timestamp    string  `json:"timestamp"`                // RFC3339 timestamp
	Event        string  `json:"event"`                    // Event type (silence_start, silence_end)
	DurationMs   int64   `json:"duration_ms,omitempty"`    // Silence duration in milliseconds (silence_end only)
	LevelLeftDB  float64 `json:"level_left_db,omitempty"`  // Left channel RMS level in dB
	LevelRightDB float64 `json:"level_right_db,omitempty"` // Right channel RMS level in dB
	ThresholdDB  float64 `json:"threshold_db"`             // Threshold in dB

	// Audio dump fields (silence_end only)
	DumpPath      string `json:"dump_path,omitempty"`       // Full path to the MP3 dump file
	DumpFilename  string `json:"dump_filename,omitempty"`   // Dump filename
	DumpSizeBytes int64  `json:"dump_size_bytes,omitempty"` // Dump file size in bytes
	DumpError     string `json:"dump_error,omitempty"`      // Error message if dump encoding failed
}

// AudioDevice represents an available audio input device.
type AudioDevice struct {
	ID   string `json:"id"`   // Device identifier
	Name string `json:"name"` // Device display name
}

// GraphConfig contains Microsoft Graph API settings for email notifications.
type GraphConfig struct {
	TenantID     string `json:"tenant_id,omitempty"`     // Azure AD tenant ID
	ClientID     string `json:"client_id,omitempty"`     // App registration client ID
	ClientSecret string `json:"client_secret,omitempty"` // App registration client secret
	FromAddress  string `json:"from_address,omitempty"`  // Shared mailbox address (sender)
	Recipients   string `json:"recipients,omitempty"`    // Comma-separated recipients
}

// ZabbixConfig contains settings for sending trapper items to a Zabbix server.
type ZabbixConfig struct {
	Server    string `json:"server,omitempty"`
	Port      int    `json:"port,omitempty"`
	Host      string `json:"host,omitempty"`
	Key       string `json:"key,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// SecretExpiryInfo contains client secret expiration data.
type SecretExpiryInfo struct {
	ExpiresAt   string `json:"expires_at,omitempty"`   // RFC3339 expiration timestamp
	ExpiresSoon bool   `json:"expires_soon,omitempty"` // True if expires within 30 days
	DaysLeft    int    `json:"days_left,omitempty"`    // Days until expiration
	Error       string `json:"error,omitempty"`        // Error message if check failed
}

// VersionInfo contains version comparison data.
type VersionInfo struct {
	Current     string `json:"current"`              // Current version
	Latest      string `json:"latest,omitempty"`     // Latest available version
	UpdateAvail bool   `json:"update_available"`     // Update is available
	Commit      string `json:"commit,omitempty"`     // Git commit hash
	BuildTime   string `json:"build_time,omitempty"` // Build timestamp
}
