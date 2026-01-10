// Package types provides shared type definitions used across the encoder.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
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

// ProcessState represents the state of any managed process (stream or recorder).
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

// ProcessStatus is the runtime status of a managed process.
type ProcessStatus struct {
	State      ProcessState `json:"state"`                 // Current process state
	Stable     bool         `json:"stable,omitempty"`      // Streams: running ≥10s
	Exhausted  bool         `json:"exhausted,omitempty"`   // Streams: max retries reached
	RetryCount int          `json:"retry_count,omitempty"` // Streams: current retry attempt
	MaxRetries int          `json:"max_retries,omitempty"` // Streams: max allowed retries
	Error      string       `json:"error,omitempty"`       // Error message
	Uptime     string       `json:"uptime,omitempty"`      // Duration since process started
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

// Codec represents an audio codec type.
type Codec string

// Supported audio codecs.
const (
	CodecWAV Codec = "wav" // Uncompressed PCM in Matroska container
	CodecMP3 Codec = "mp3" // MPEG Audio Layer III
	CodecMP2 Codec = "mp2" // MPEG Audio Layer II
	CodecOGG Codec = "ogg" // Ogg Vorbis
)

// ValidCodecs is the set of supported audio codecs.
var ValidCodecs = map[Codec]bool{
	CodecWAV: true, CodecMP3: true, CodecMP2: true, CodecOGG: true,
}

// UnmarshalJSON implements json.Unmarshaler for strict parsing.
func (c *Codec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*c = CodecWAV // default
		return nil
	}
	codec := Codec(s)
	if !ValidCodecs[codec] {
		return fmt.Errorf("codec: must be wav, mp3, mp2, or ogg")
	}
	*c = codec
	return nil
}

// Stream represents a single SRT streaming destination.
type Stream struct {
	ID         string `json:"id"`          // Unique identifier
	Enabled    bool   `json:"enabled"`     // Whether stream is active
	Host       string `json:"host"`        // SRT server hostname
	Port       int    `json:"port"`        // SRT server port
	Password   string `json:"password"`    // SRT encryption passphrase
	StreamID   string `json:"stream_id"`   // SRT stream identifier
	Codec      Codec  `json:"codec"`       // Audio codec (mp2, mp3, ogg, wav)
	MaxRetries int    `json:"max_retries"` // Maximum retry attempts (0 = no retries)
	CreatedAt  int64  `json:"created_at"`  // Unix timestamp of creation
}

// IsEnabled reports whether the stream is enabled.
func (s *Stream) IsEnabled() bool {
	return s.Enabled
}

// DefaultMaxRetries is the default number of retry attempts for streams.
const DefaultMaxRetries = 99

// StreamRestartDelay is the delay between stopping and starting a stream during restart.
const StreamRestartDelay = 2000 * time.Millisecond

// MaxRetriesOrDefault returns the configured max retries or the default value.
func (s *Stream) MaxRetriesOrDefault() int {
	if s.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return s.MaxRetries
}

// CodecPreset defines FFmpeg encoding parameters for a codec.
type CodecPreset struct {
	Args   []string // FFmpeg codec arguments
	Format string   // FFmpeg output format
}

// CodecPresets is the FFmpeg configuration for each codec type.
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

// CodecArgs returns FFmpeg codec arguments for this stream's codec.
func (s *Stream) CodecArgs() []string {
	return CodecArgsFor(s.Codec)
}

// Format returns the FFmpeg output format for this stream's codec.
func (s *Stream) Format() string {
	return FormatFor(s.Codec)
}

// Validate reports an error if the stream configuration is invalid.
// Note: Codec validation is handled by UnmarshalJSON during parsing.
func (s *Stream) Validate() error {
	if strings.TrimSpace(s.Host) == "" {
		return fmt.Errorf("host: is required")
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("port: must be between 1 and 65535")
	}
	if s.MaxRetries < 0 {
		return fmt.Errorf("max_retries: cannot be negative")
	}
	return nil
}

// RotationMode is the strategy for splitting recordings into files.
type RotationMode string

// Supported rotation modes.
const (
	RotationHourly RotationMode = "hourly" // Rotate at system clock hour boundaries
)

// ValidRotationModes is the set of supported rotation modes.
var ValidRotationModes = map[RotationMode]bool{
	RotationHourly: true,
}

// UnmarshalJSON implements json.Unmarshaler for strict parsing.
func (m *RotationMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*m = RotationHourly // default
		return nil
	}
	mode := RotationMode(s)
	if !ValidRotationModes[mode] {
		return fmt.Errorf("rotation_mode: must be hourly")
	}
	*m = mode
	return nil
}

// StorageMode is the storage destination type for recordings.
type StorageMode string

// Supported storage modes.
const (
	StorageLocal StorageMode = "local" // Save only to local filesystem
	StorageS3    StorageMode = "s3"    // Upload only to S3
	StorageBoth  StorageMode = "both"  // Save locally AND upload to S3
)

// ValidStorageModes is the set of supported storage modes.
var ValidStorageModes = map[StorageMode]bool{
	StorageLocal: true, StorageS3: true, StorageBoth: true,
}

// UnmarshalJSON implements json.Unmarshaler for strict parsing.
func (m *StorageMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*m = StorageLocal // default
		return nil
	}
	mode := StorageMode(s)
	if !ValidStorageModes[mode] {
		return fmt.Errorf("storage_mode: must be local, s3, or both")
	}
	*m = mode
	return nil
}

// DefaultRetentionDays is the default number of days to keep recordings.
const DefaultRetentionDays = 90

// DefaultSilenceDumpRetentionDays is the default number of days to keep silence dumps.
const DefaultSilenceDumpRetentionDays = 7

// SilenceDumpConfig is the configuration for silence dump capture.
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

// Validate reports an error if the recorder configuration is invalid.
// Note: Codec, RotationMode, StorageMode validation is handled by UnmarshalJSON during parsing.
func (r *Recorder) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name: is required")
	}

	// Conditional validation based on storage mode
	needsLocal := r.StorageMode == StorageLocal || r.StorageMode == StorageBoth
	needsS3 := r.StorageMode == StorageS3 || r.StorageMode == StorageBoth

	if needsLocal && strings.TrimSpace(r.LocalPath) == "" {
		return fmt.Errorf("local_path: is required for local/both storage mode")
	}
	if needsS3 {
		if r.S3Bucket == "" {
			return fmt.Errorf("s3_bucket: is required for s3/both storage mode")
		}
		if r.S3AccessKeyID == "" {
			return fmt.Errorf("s3_access_key_id: is required for s3/both storage mode")
		}
		if r.S3SecretAccessKey == "" {
			return fmt.Errorf("s3_secret_access_key: is required for s3/both storage mode")
		}
	}
	if r.RetentionDays < 0 {
		return fmt.Errorf("retention_days: cannot be negative")
	}
	return nil
}

// EncoderStatus is a summary of the encoder's current operational state.
type EncoderStatus struct {
	State            EncoderState `json:"state"`                       // Current encoder state
	Uptime           string       `json:"uptime,omitzero"`             // Time since start
	LastError        string       `json:"last_error,omitzero"`         // Most recent error
	StreamCount      int          `json:"stream_count"`                // Number of streams
	SourceRetryCount int          `json:"source_retry_count,omitzero"` // Source retry attempts
	SourceMaxRetries int          `json:"source_max_retries"`          // Max source retries
}

// WSRuntimeStatus is sent to clients with only runtime status (no config).
// This is the lightweight status message sent every 3 seconds via WebSocket.
type WSRuntimeStatus struct {
	Type              string                   `json:"type"`                // Message type identifier ("status")
	FFmpegAvailable   bool                     `json:"ffmpeg_available"`    // FFmpeg binary is available
	Encoder           EncoderStatus            `json:"encoder"`             // Encoder status
	StreamStatus      map[string]ProcessStatus `json:"stream_status"`       // Runtime stream status by ID
	RecorderStatuses  map[string]ProcessStatus `json:"recorder_statuses"`   // Runtime recorder status by ID
	GraphSecretExpiry SecretExpiryInfo         `json:"graph_secret_expiry"` // Client secret expiration info
	Version           VersionInfo              `json:"version"`             // Version information
}

// APIConfigResponse is returned by GET /api/config with full configuration.
type APIConfigResponse struct {
	// Audio settings
	AudioInput string         `json:"audio_input"` // Selected audio input device
	Devices    []audio.Device `json:"devices"`     // Available audio devices
	Platform   string         `json:"platform"`    // Operating system platform

	// Silence detection
	SilenceThreshold  float64           `json:"silence_threshold"`   // Silence threshold in dB
	SilenceDurationMs int64             `json:"silence_duration_ms"` // Silence duration in milliseconds
	SilenceRecoveryMs int64             `json:"silence_recovery_ms"` // Recovery duration in milliseconds
	SilenceDump       SilenceDumpConfig `json:"silence_dump"`        // Silence dump configuration

	// Notifications - Webhook
	WebhookURL string `json:"webhook_url"` // Webhook URL for alerts

	// Notifications - Zabbix
	ZabbixServer string `json:"zabbix_server"` // Zabbix server address
	ZabbixPort   int    `json:"zabbix_port"`   // Zabbix server port
	ZabbixHost   string `json:"zabbix_host"`   // Zabbix host name
	ZabbixKey    string `json:"zabbix_key"`    // Zabbix item key

	// Notifications - Email (Microsoft Graph)
	GraphTenantID    string `json:"graph_tenant_id"`    // Azure AD tenant ID
	GraphClientID    string `json:"graph_client_id"`    // App registration client ID
	GraphFromAddress string `json:"graph_from_address"` // Shared mailbox address
	GraphRecipients  string `json:"graph_recipients"`   // Comma-separated recipients
	GraphHasSecret   bool   `json:"graph_has_secret"`   // Whether client secret is configured

	// Recording
	RecordingAPIKey string `json:"recording_api_key"` // API key for recording control

	// Entities
	Streams   []Stream   `json:"streams"`   // Stream configurations
	Recorders []Recorder `json:"recorders"` // Recorder configurations
}

// WSLevelsResponse is sent to clients with audio level updates.
type WSLevelsResponse struct {
	Type   string            `json:"type"`   // Message type identifier
	Levels audio.AudioLevels `json:"levels"` // Current audio levels
}

// GraphConfig is the Microsoft Graph API configuration for email notifications.
type GraphConfig struct {
	TenantID     string `json:"tenant_id,omitempty"`     // Azure AD tenant ID
	ClientID     string `json:"client_id,omitempty"`     // App registration client ID
	ClientSecret string `json:"client_secret,omitempty"` // App registration client secret
	FromAddress  string `json:"from_address,omitempty"`  // Shared mailbox address (sender)
	Recipients   string `json:"recipients,omitempty"`    // Comma-separated recipients
}

// ZabbixConfig is the configuration for sending trapper items to a Zabbix server.
type ZabbixConfig struct {
	Server string `json:"server,omitempty"`
	Port   int    `json:"port,omitempty"`
	Host   string `json:"host,omitempty"`
	Key    string `json:"key,omitempty"`
}

// SecretExpiryInfo is the expiration status of a client secret.
type SecretExpiryInfo struct {
	ExpiresAt   string `json:"expires_at,omitempty"`   // RFC3339 expiration timestamp
	ExpiresSoon bool   `json:"expires_soon,omitempty"` // True if expires within 30 days
	DaysLeft    int    `json:"days_left,omitempty"`    // Days until expiration
	Error       string `json:"error,omitempty"`        // Error message if check failed
}

// VersionInfo is the current and latest version information.
type VersionInfo struct {
	Current     string `json:"current"`              // Current version
	Latest      string `json:"latest,omitempty"`     // Latest available version
	UpdateAvail bool   `json:"update_available"`     // Update is available
	Commit      string `json:"commit,omitempty"`     // Git commit hash
	BuildTime   string `json:"build_time,omitempty"` // Build timestamp
}
