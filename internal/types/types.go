// Package types defines shared types and constants used across the encoder.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// EncoderState describes the encoder lifecycle phase.
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

// ProcessState describes the lifecycle phase of a stream or recorder.
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

// ProcessStatus holds runtime status for a stream or recorder.
type ProcessStatus struct {
	// State is the current process state.
	State ProcessState `json:"state"`
	// Stable reports whether a stream has been running long enough to be stable.
	Stable bool `json:"stable,omitempty"`
	// Exhausted reports whether retries are exhausted.
	Exhausted bool `json:"exhausted,omitempty"`
	// RetryCount is the current retry attempt.
	RetryCount int `json:"retry_count,omitempty"`
	// MaxRetries is the maximum allowed retry attempts.
	MaxRetries int `json:"max_retries,omitempty"`
	// Error is the last error message.
	Error string `json:"error,omitempty"`
	// Uptime is the time since the process started.
	Uptime string `json:"uptime,omitempty"`
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

// Codec identifies an audio encoding format.
type Codec string

const (
	// CodecWAV is uncompressed PCM in a Matroska container.
	CodecWAV Codec = "wav"
	// CodecMP3 is MPEG Audio Layer III.
	CodecMP3 Codec = "mp3"
	// CodecMP2 is MPEG Audio Layer II.
	CodecMP2 Codec = "mp2"
	// CodecOGG is Ogg Vorbis.
	CodecOGG Codec = "ogg"
)

// ValidCodecs is the set of supported audio codecs.
var ValidCodecs = map[Codec]bool{
	CodecWAV: true, CodecMP3: true, CodecMP2: true, CodecOGG: true,
}

// UnmarshalJSON validates the codec value during JSON parsing.
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

// Stream defines an SRT streaming destination.
type Stream struct {
	// ID is the unique stream identifier.
	ID string `json:"id"`
	// Enabled reports whether the stream is active.
	Enabled bool `json:"enabled"`
	// Host is the SRT server hostname.
	Host string `json:"host"`
	// Port is the SRT server port.
	Port int `json:"port"`
	// Password is the SRT encryption passphrase.
	Password string `json:"password"`
	// StreamID is the SRT stream identifier.
	StreamID string `json:"stream_id"`
	// Codec selects the audio codec.
	Codec Codec `json:"codec"`
	// MaxRetries is the maximum retry attempts (0 = no retries).
	MaxRetries int `json:"max_retries"`
	// CreatedAt is the Unix timestamp of creation.
	CreatedAt int64 `json:"created_at"`
}

// IsEnabled reports whether the stream is enabled.
func (s *Stream) IsEnabled() bool {
	return s.Enabled
}

// DefaultMaxRetries is the default number of retry attempts for streams.
const DefaultMaxRetries = 99

// StreamRestartDelay is the delay between stopping and starting a stream during restart.
const StreamRestartDelay = 2000 * time.Millisecond

// MaxRetriesOrDefault returns MaxRetries, or [DefaultMaxRetries] if not set.
func (s *Stream) MaxRetriesOrDefault() int {
	if s.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return s.MaxRetries
}

// CodecPreset defines encoding parameters for a codec.
type CodecPreset struct {
	// Args are encoder arguments.
	Args []string
	// Format is the output container format.
	Format string
}

// CodecPresets maps codecs to their encoding parameters.
var CodecPresets = map[Codec]CodecPreset{
	CodecMP2: {[]string{"libtwolame", "-b:a", "384k", "-psymodel", "4"}, "mp2"},
	CodecMP3: {[]string{"libmp3lame", "-b:a", "320k"}, "mp3"},
	CodecOGG: {[]string{"libvorbis", "-qscale:a", "10"}, "ogg"},
	CodecWAV: {[]string{"pcm_s16le"}, "matroska"},
}

// Args returns the encoder arguments for this codec.
func (c Codec) Args() []string {
	if preset, ok := CodecPresets[c]; ok {
		return preset.Args
	}
	return CodecPresets[CodecWAV].Args
}

// Format returns the output format for this codec.
func (c Codec) Format() string {
	if preset, ok := CodecPresets[c]; ok {
		return preset.Format
	}
	return CodecPresets[CodecWAV].Format
}

// CodecArgs returns the encoder arguments for this stream's codec.
func (s *Stream) CodecArgs() []string {
	return s.Codec.Args()
}

// Format returns the output format for this stream's codec.
func (s *Stream) Format() string {
	return s.Codec.Format()
}

// Validate reports an error if the stream configuration is invalid.
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

// RotationMode defines how recordings are split into files.
type RotationMode string

const (
	// RotationHourly rotates at system clock hour boundaries.
	RotationHourly RotationMode = "hourly"
)

// ValidRotationModes is the set of supported rotation modes.
var ValidRotationModes = map[RotationMode]bool{
	RotationHourly: true,
}

// UnmarshalJSON validates the rotation mode during JSON parsing.
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

// StorageMode defines where recordings are stored.
type StorageMode string

const (
	// StorageLocal saves only to the local filesystem.
	StorageLocal StorageMode = "local"
	// StorageS3 uploads only to S3.
	StorageS3 StorageMode = "s3"
	// StorageBoth saves locally and uploads to S3.
	StorageBoth StorageMode = "both"
)

// ValidStorageModes is the set of supported storage modes.
var ValidStorageModes = map[StorageMode]bool{
	StorageLocal: true, StorageS3: true, StorageBoth: true,
}

// UnmarshalJSON validates the storage mode during JSON parsing.
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

// SilenceDumpConfig defines settings for capturing audio around silence events.
type SilenceDumpConfig struct {
	// Enabled reports whether dump capture is active.
	Enabled bool `json:"enabled"`
	// RetentionDays is the number of days to keep dump files.
	RetentionDays int `json:"retention_days"`
}

// Recorder defines a recording destination configuration.
type Recorder struct {
	// ID is the unique recorder identifier.
	ID string `json:"id"`
	// Name is the recorder display name.
	Name string `json:"name"`
	// Enabled reports whether the recorder is active.
	Enabled bool `json:"enabled"`
	// Codec selects the audio codec.
	Codec Codec `json:"codec"`
	// RotationMode selects the rotation mode.
	RotationMode RotationMode `json:"rotation_mode"`
	// StorageMode selects the storage mode.
	StorageMode StorageMode `json:"storage_mode"`
	// LocalPath is the local directory for recordings.
	LocalPath string `json:"local_path"`

	// S3Endpoint is the S3-compatible endpoint URL.
	S3Endpoint string `json:"s3_endpoint"`
	// S3Bucket is the S3 bucket name.
	S3Bucket string `json:"s3_bucket"`
	// S3AccessKeyID is the S3 access key ID.
	S3AccessKeyID string `json:"s3_access_key_id"`
	// S3SecretAccessKey is the S3 secret access key.
	S3SecretAccessKey string `json:"s3_secret_access_key"`

	// RetentionDays is the number of days to keep recordings.
	RetentionDays int `json:"retention_days"`
	// CreatedAt is the Unix timestamp of creation.
	CreatedAt int64 `json:"created_at"`
}

// IsEnabled reports whether the recorder is enabled.
func (r *Recorder) IsEnabled() bool {
	return r.Enabled
}

// CodecArgs returns the encoder arguments for this recorder's codec.
func (r *Recorder) CodecArgs() []string {
	return r.Codec.Args()
}

// Format returns the output format for this recorder's codec.
func (r *Recorder) Format() string {
	return r.Codec.Format()
}

// Validate reports an error if the recorder configuration is invalid.
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

// EncoderStatus summarizes the encoder's current operational state.
type EncoderStatus struct {
	// State is the current encoder state.
	State EncoderState `json:"state"`
	// Uptime is the time since start in human-readable form.
	Uptime string `json:"uptime,omitzero"`
	// UptimeSeconds is the time since start in seconds.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LastError is the most recent error message.
	LastError string `json:"last_error,omitzero"`
	// StreamCount is the number of configured streams.
	StreamCount int `json:"stream_count"`
	// SourceRetryCount is the source retry attempt count.
	SourceRetryCount int `json:"source_retry_count,omitzero"`
	// SourceMaxRetries is the maximum source retry attempts.
	SourceMaxRetries int `json:"source_max_retries"`
}

// WSRuntimeStatus contains runtime status sent to clients periodically.
type WSRuntimeStatus struct {
	// Type is the message type identifier.
	Type string `json:"type"`
	// FFmpegAvailable reports whether the FFmpeg binary is available.
	FFmpegAvailable bool `json:"ffmpeg_available"`
	// Encoder holds encoder status.
	Encoder EncoderStatus `json:"encoder"`
	// StreamStatus holds runtime stream status by ID.
	StreamStatus map[string]ProcessStatus `json:"stream_status"`
	// RecorderStatuses holds runtime recorder status by ID.
	RecorderStatuses map[string]ProcessStatus `json:"recorder_statuses"`
	// GraphSecretExpiry holds client secret expiration info.
	GraphSecretExpiry SecretExpiryInfo `json:"graph_secret_expiry"`
	// Version holds version information.
	Version VersionInfo `json:"version"`
}

// APIConfigResponse contains the complete encoder configuration for API responses.
type APIConfigResponse struct {
	// AudioInput is the selected audio input device.
	AudioInput string `json:"audio_input"`
	// Devices lists available audio devices.
	Devices []audio.Device `json:"devices"`
	// Platform is the operating system platform.
	Platform string `json:"platform"`

	// SilenceThreshold is the silence threshold in dB.
	SilenceThreshold float64 `json:"silence_threshold"`
	// SilenceDurationMs is the silence duration in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms"`
	// SilenceRecoveryMs is the recovery duration in milliseconds.
	SilenceRecoveryMs int64 `json:"silence_recovery_ms"`
	// SilenceDump holds silence dump configuration.
	SilenceDump SilenceDumpConfig `json:"silence_dump"`

	// WebhookURL is the webhook URL for alerts.
	WebhookURL string `json:"webhook_url"`

	// ZabbixServer is the Zabbix server address.
	ZabbixServer string `json:"zabbix_server"`
	// ZabbixPort is the Zabbix server port.
	ZabbixPort int `json:"zabbix_port"`
	// ZabbixHost is the Zabbix host name.
	ZabbixHost string `json:"zabbix_host"`
	// ZabbixKey is the Zabbix item key.
	ZabbixKey string `json:"zabbix_key"`

	// GraphTenantID is the Azure AD tenant ID.
	GraphTenantID string `json:"graph_tenant_id"`
	// GraphClientID is the app registration client ID.
	GraphClientID string `json:"graph_client_id"`
	// GraphFromAddress is the shared mailbox sender address.
	GraphFromAddress string `json:"graph_from_address"`
	// GraphRecipients is a comma-separated recipient list.
	GraphRecipients string `json:"graph_recipients"`
	// GraphHasSecret reports whether a client secret is configured.
	GraphHasSecret bool `json:"graph_has_secret"`

	// RecordingAPIKey is the API key for recording control.
	RecordingAPIKey string `json:"recording_api_key"`

	// Streams lists stream configurations.
	Streams []Stream `json:"streams"`
	// Recorders lists recorder configurations.
	Recorders []Recorder `json:"recorders"`
}

// WSLevelsResponse contains audio level data sent to clients.
type WSLevelsResponse struct {
	// Type is the message type identifier.
	Type string `json:"type"`
	// Levels are the current audio levels.
	Levels audio.AudioLevels `json:"levels"`
}

// GraphConfig holds credentials for email notifications.
type GraphConfig struct {
	// TenantID is the Azure AD tenant ID.
	TenantID string `json:"tenant_id,omitempty"`
	// ClientID is the app registration client ID.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is the app registration client secret.
	ClientSecret string `json:"client_secret,omitempty"`
	// FromAddress is the shared mailbox sender address.
	FromAddress string `json:"from_address,omitempty"`
	// Recipients is a comma-separated recipient list.
	Recipients string `json:"recipients,omitempty"`
}

// ZabbixConfig holds settings for external monitoring alerts.
type ZabbixConfig struct {
	// Server is the Zabbix server host.
	Server string `json:"server,omitempty"`
	// Port is the Zabbix server port.
	Port int `json:"port,omitempty"`
	// Host is the Zabbix host name.
	Host string `json:"host,omitempty"`
	// Key is the Zabbix item key.
	Key string `json:"key,omitempty"`
}

// SecretExpiryInfo holds expiration details for a client secret.
type SecretExpiryInfo struct {
	// ExpiresAt is the RFC3339 expiration timestamp.
	ExpiresAt string `json:"expires_at,omitempty"`
	// ExpiresSoon reports whether expiration is within 30 days.
	ExpiresSoon bool `json:"expires_soon,omitempty"`
	// DaysLeft is the number of days until expiration.
	DaysLeft int `json:"days_left,omitempty"`
	// Error is the error message if the check failed.
	Error string `json:"error,omitempty"`
}

// VersionInfo holds current and latest version details.
type VersionInfo struct {
	// Current is the current version.
	Current string `json:"current"`
	// Latest is the latest available version.
	Latest string `json:"latest,omitempty"`
	// UpdateAvail reports whether an update is available.
	UpdateAvail bool `json:"update_available"`
	// Commit is the git commit hash.
	Commit string `json:"commit,omitempty"`
	// BuildTime is the build timestamp.
	BuildTime string `json:"build_time,omitempty"`
}
