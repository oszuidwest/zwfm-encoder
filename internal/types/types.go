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
	// State is the lifecycle phase (stopped, running, error, etc.).
	State ProcessState `json:"state"`
	// Stable reports whether the stream has been running past the stability threshold.
	Stable bool `json:"stable,omitempty"`
	// Exhausted reports whether all retry attempts have been used.
	Exhausted bool `json:"exhausted,omitempty"`
	// RetryCount is which retry attempt is currently in progress.
	RetryCount int `json:"retry_count,omitempty"`
	// MaxRetries is the configured maximum retry attempts before giving up.
	MaxRetries int `json:"max_retries,omitempty"`
	// Error is the most recent error message, if any.
	Error string `json:"error,omitempty"`
	// Uptime is the elapsed time since start in human-readable form (e.g., "1h23m").
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
	// ID is the unique identifier assigned when the stream is created.
	ID string `json:"id"`
	// Enabled reports whether the stream should be started with the encoder.
	Enabled bool `json:"enabled"`
	// Host is the SRT server hostname or IP address to connect to.
	Host string `json:"host"`
	// Port is the TCP port on the SRT server to connect to.
	Port int `json:"port"`
	// Password is the SRT encryption passphrase, if required by the server.
	Password string `json:"password"`
	// StreamID is the SRT stream identifier sent to the server for routing.
	StreamID string `json:"stream_id"`
	// Codec selects the audio encoding format (mp3, mp2, ogg, wav).
	Codec Codec `json:"codec"`
	// MaxRetries is how many reconnection attempts before giving up (0 = no retries).
	MaxRetries int `json:"max_retries"`
	// CreatedAt is the Unix timestamp in milliseconds when the stream was created.
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
	// Args contains the FFmpeg encoder arguments (e.g., "libmp3lame", "-b:a", "320k").
	Args []string
	// Format is the output container format (e.g., "mp3", "ogg", "matroska").
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
	// RotationHourly rotates recordings at system clock hour boundaries.
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
	// StorageLocal stores recordings on the local filesystem only.
	StorageLocal StorageMode = "local"
	// StorageS3 uploads recordings to S3-compatible storage only.
	StorageS3 StorageMode = "s3"
	// StorageBoth stores recordings locally and uploads to S3-compatible storage.
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
	// Enabled reports whether audio dumps are captured during silence events.
	Enabled bool `json:"enabled"`
	// RetentionDays is how many days to keep dump files before cleanup (0 = forever).
	RetentionDays int `json:"retention_days"`
}

// Recorder defines a recording destination configuration.
type Recorder struct {
	// ID is the unique identifier assigned when the recorder is created.
	ID string `json:"id"`
	// Name is the display name shown in the web UI.
	Name string `json:"name"`
	// Enabled reports whether the recorder should be started with the encoder.
	Enabled bool `json:"enabled"`
	// Codec selects the audio encoding format (mp3, mp2, ogg, wav).
	Codec Codec `json:"codec"`
	// RotationMode selects when recordings are split into new files (hourly).
	RotationMode RotationMode `json:"rotation_mode"`
	// StorageMode selects where recordings are stored (local, s3, both).
	StorageMode StorageMode `json:"storage_mode"`
	// LocalPath is the directory where local recordings are saved.
	LocalPath string `json:"local_path"`

	// S3Endpoint is the S3-compatible storage endpoint URL.
	S3Endpoint string `json:"s3_endpoint"`
	// S3Bucket is the bucket name where recordings are uploaded.
	S3Bucket string `json:"s3_bucket"`
	// S3AccessKeyID is the access key for S3 authentication.
	S3AccessKeyID string `json:"s3_access_key_id"`
	// S3SecretAccessKey is the secret key for S3 authentication.
	S3SecretAccessKey string `json:"s3_secret_access_key"`

	// RetentionDays is how many days to keep recordings before cleanup (0 = forever).
	RetentionDays int `json:"retention_days"`
	// CreatedAt is the Unix timestamp in milliseconds when the recorder was created.
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
	// State is the lifecycle phase (stopped, starting, running, stopping).
	State EncoderState `json:"state"`
	// Uptime is the elapsed time since start in human-readable form (e.g., "1h23m").
	Uptime string `json:"uptime,omitzero"`
	// UptimeSeconds is the elapsed time since start in seconds.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LastError is the most recent error message, if any.
	LastError string `json:"last_error,omitzero"`
	// StreamCount is how many streams are configured.
	StreamCount int `json:"stream_count"`
	// SourceRetryCount is which audio source retry attempt is in progress.
	SourceRetryCount int `json:"source_retry_count,omitzero"`
	// SourceMaxRetries is the configured maximum audio source retry attempts.
	SourceMaxRetries int `json:"source_max_retries"`
}

// WSRuntimeStatus contains runtime status sent to clients periodically.
type WSRuntimeStatus struct {
	// Type is the WebSocket message type (always "status").
	Type string `json:"type"`
	// FFmpegAvailable reports whether the FFmpeg binary was found at startup.
	FFmpegAvailable bool `json:"ffmpeg_available"`
	// Encoder holds the encoder's current status.
	Encoder EncoderStatus `json:"encoder"`
	// StreamStatus maps stream IDs to their current runtime status.
	StreamStatus map[string]ProcessStatus `json:"stream_status"`
	// RecorderStatuses maps recorder IDs to their current runtime status.
	RecorderStatuses map[string]ProcessStatus `json:"recorder_statuses"`
	// GraphSecretExpiry holds Azure app client secret expiration info.
	GraphSecretExpiry SecretExpiryInfo `json:"graph_secret_expiry"`
	// Version holds current and latest version details.
	Version VersionInfo `json:"version"`
}

// APIConfigResponse contains the complete encoder configuration for API responses.
type APIConfigResponse struct {
	// AudioInput is the configured audio input device identifier.
	AudioInput string `json:"audio_input"`
	// Devices lists all available audio input devices on this system.
	Devices []audio.Device `json:"devices"`
	// Platform is the operating system (linux, darwin, windows).
	Platform string `json:"platform"`

	// SilenceThreshold is the audio level in dB below which silence is detected.
	SilenceThreshold float64 `json:"silence_threshold"`
	// SilenceDurationMs is how long audio must be below threshold before alerting.
	SilenceDurationMs int64 `json:"silence_duration_ms"`
	// SilenceRecoveryMs is how long audio must be above threshold before clearing the alert.
	SilenceRecoveryMs int64 `json:"silence_recovery_ms"`
	// SilenceDump holds silence audio dump configuration.
	SilenceDump SilenceDumpConfig `json:"silence_dump"`

	// WebhookURL is the endpoint to POST silence alerts to.
	WebhookURL string `json:"webhook_url"`

	// ZabbixServer is the Zabbix trapper server hostname or IP.
	ZabbixServer string `json:"zabbix_server"`
	// ZabbixPort is the Zabbix trapper server port.
	ZabbixPort int `json:"zabbix_port"`
	// ZabbixHost is the host name as registered in Zabbix.
	ZabbixHost string `json:"zabbix_host"`
	// ZabbixKey is the item key for Zabbix trapper values.
	ZabbixKey string `json:"zabbix_key"`

	// GraphTenantID is the Azure AD tenant ID for Graph API authentication.
	GraphTenantID string `json:"graph_tenant_id"`
	// GraphClientID is the Azure app registration client ID.
	GraphClientID string `json:"graph_client_id"`
	// GraphFromAddress is the shared mailbox address to send emails from.
	GraphFromAddress string `json:"graph_from_address"`
	// GraphRecipients is a comma-separated list of email addresses to notify.
	GraphRecipients string `json:"graph_recipients"`
	// GraphHasSecret reports whether an Azure client secret is configured.
	GraphHasSecret bool `json:"graph_has_secret"`

	// RecordingAPIKey is the secret key for external recording control via REST API.
	RecordingAPIKey string `json:"recording_api_key"`

	// Streams lists all configured stream destinations.
	Streams []Stream `json:"streams"`
	// Recorders lists all configured recording destinations.
	Recorders []Recorder `json:"recorders"`
}

// WSLevelsResponse contains audio level data sent to clients.
type WSLevelsResponse struct {
	// Type is the WebSocket message type (always "levels").
	Type string `json:"type"`
	// Levels contains current RMS and peak audio levels for VU meters.
	Levels audio.AudioLevels `json:"levels"`
}

// GraphConfig holds credentials for Microsoft Graph email notifications.
type GraphConfig struct {
	// TenantID is the Azure AD tenant ID for Graph API authentication.
	TenantID string `json:"tenant_id,omitempty"`
	// ClientID is the Azure app registration client ID.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is the Azure app registration client secret.
	ClientSecret string `json:"client_secret,omitempty"`
	// FromAddress is the shared mailbox address to send emails from.
	FromAddress string `json:"from_address,omitempty"`
	// Recipients is a comma-separated list of email addresses to notify.
	Recipients string `json:"recipients,omitempty"`
}

// ZabbixConfig holds settings for Zabbix trapper monitoring alerts.
type ZabbixConfig struct {
	// Server is the Zabbix trapper server hostname or IP.
	Server string `json:"server,omitempty"`
	// Port is the Zabbix trapper server port (default 10051).
	Port int `json:"port,omitempty"`
	// Host is the host name as registered in Zabbix.
	Host string `json:"host,omitempty"`
	// Key is the item key for Zabbix trapper values.
	Key string `json:"key,omitempty"`
}

// SecretExpiryInfo holds expiration details for an Azure client secret.
type SecretExpiryInfo struct {
	// ExpiresAt is when the secret expires in RFC3339 format.
	ExpiresAt string `json:"expires_at,omitempty"`
	// ExpiresSoon reports whether expiration is within 30 days.
	ExpiresSoon bool `json:"expires_soon,omitempty"`
	// DaysLeft is how many days remain until expiration.
	DaysLeft int `json:"days_left,omitempty"`
	// Error is the error message if the expiry check failed.
	Error string `json:"error,omitempty"`
}

// VersionInfo holds current and latest version details.
type VersionInfo struct {
	// Current is the running encoder version (e.g., "v1.2.3").
	Current string `json:"current"`
	// Latest is the newest release version available on GitHub.
	Latest string `json:"latest,omitempty"`
	// UpdateAvail reports whether a newer version is available on GitHub.
	UpdateAvail bool `json:"update_available"`
	// Commit is the git commit hash this build was made from.
	Commit string `json:"commit,omitempty"`
	// BuildTime is when this binary was built in RFC3339 format.
	BuildTime string `json:"build_time,omitempty"`
}
