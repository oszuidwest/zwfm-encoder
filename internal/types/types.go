// Package types defines shared types and constants used across the encoder.
package types

import (
	"encoding/json"
	"fmt"
	"strconv"
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
	State      ProcessState `json:"state"`
	Stable     bool         `json:"stable,omitempty"`
	Exhausted  bool         `json:"exhausted,omitempty"`
	RetryCount int          `json:"retry_count,omitempty"`
	MaxRetries int          `json:"max_retries,omitempty"`
	Error      string       `json:"error,omitempty"`
	Uptime     string       `json:"uptime,omitempty"`
	AudioDrops int64        `json:"audio_drops,omitempty"`
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
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	StreamID   string `json:"stream_id"`
	Codec      Codec  `json:"codec"`
	Bitrate    int    `json:"bitrate"`     // kbit/s, 0 = codec default
	MaxRetries int    `json:"max_retries"` // 0 = no retries
	CreatedAt  int64  `json:"created_at"`  // Unix ms
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
	Args   []string
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

// BuildCodecArgs returns FFmpeg encoder arguments for the given codec and bitrate.
// A bitrate of 0 uses the codec's default settings.
func BuildCodecArgs(codec Codec, bitrate int) []string {
	switch codec {
	case CodecMP3:
		br := "320k"
		if bitrate > 0 {
			br = strconv.Itoa(bitrate) + "k"
		}
		return []string{"libmp3lame", "-b:a", br}
	case CodecMP2:
		br := "384k"
		if bitrate > 0 {
			br = strconv.Itoa(bitrate) + "k"
		}
		return []string{"libtwolame", "-b:a", br, "-psymodel", "4"}
	case CodecOGG:
		if bitrate > 0 {
			return []string{"libvorbis", "-b:a", strconv.Itoa(bitrate) + "k"}
		}
		return []string{"libvorbis", "-qscale:a", "10"}
	default:
		return []string{"pcm_s16le"}
	}
}

// ValidateBitrate checks whether the bitrate is valid for the given codec.
// A bitrate of 0 is always valid (uses codec default).
func ValidateBitrate(codec Codec, bitrate int) error {
	if bitrate == 0 {
		return nil
	}
	switch codec {
	case CodecMP3:
		if bitrate < 64 || bitrate > 320 {
			return fmt.Errorf("bitrate: must be between 64 and 320 for MP3")
		}
	case CodecMP2:
		if bitrate < 64 || bitrate > 384 {
			return fmt.Errorf("bitrate: must be between 64 and 384 for MP2")
		}
	case CodecOGG:
		if bitrate < 64 || bitrate > 500 {
			return fmt.Errorf("bitrate: must be between 64 and 500 for OGG")
		}
	case CodecWAV:
		return fmt.Errorf("bitrate: not supported for WAV (uncompressed)")
	}
	return nil
}

// CodecArgs returns the encoder arguments for this stream's codec and bitrate.
func (s *Stream) CodecArgs() []string {
	return BuildCodecArgs(s.Codec, s.Bitrate)
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
	if err := ValidateBitrate(s.Codec, s.Bitrate); err != nil {
		return err
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
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"` // 0 = forever
}

// Recorder defines a recording destination configuration.
type Recorder struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Enabled      bool         `json:"enabled"`
	Codec        Codec        `json:"codec"`
	Bitrate      int          `json:"bitrate"` // kbit/s, 0 = codec default
	RotationMode RotationMode `json:"rotation_mode"`
	StorageMode  StorageMode  `json:"storage_mode"`
	LocalPath    string       `json:"local_path"`

	S3Endpoint        string `json:"s3_endpoint"`
	S3Bucket          string `json:"s3_bucket"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`

	RetentionDays int   `json:"retention_days"` // 0 = forever
	CreatedAt     int64 `json:"created_at"`     // Unix ms
}

// IsEnabled reports whether the recorder is enabled.
func (r *Recorder) IsEnabled() bool {
	return r.Enabled
}

// CodecArgs returns the encoder arguments for this recorder's codec and bitrate.
func (r *Recorder) CodecArgs() []string {
	return BuildCodecArgs(r.Codec, r.Bitrate)
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
	if err := ValidateBitrate(r.Codec, r.Bitrate); err != nil {
		return err
	}
	return nil
}

// EncoderStatus summarizes the encoder's current operational state.
type EncoderStatus struct {
	State            EncoderState `json:"state"`
	Uptime           string       `json:"uptime,omitzero"`
	UptimeSeconds    int64        `json:"uptime_seconds"`
	LastError        string       `json:"last_error,omitzero"`
	StreamCount      int          `json:"stream_count"`
	SourceRetryCount int          `json:"source_retry_count,omitzero"`
	SourceMaxRetries int          `json:"source_max_retries"`
}

// WSRuntimeStatus contains runtime status sent to clients periodically.
type WSRuntimeStatus struct {
	Type              string                   `json:"type"` // Always "status"
	FFmpegAvailable   bool                     `json:"ffmpeg_available"`
	Encoder           EncoderStatus            `json:"encoder"`
	StreamStatus      map[string]ProcessStatus `json:"stream_status"`
	RecorderStatuses  map[string]ProcessStatus `json:"recorder_statuses"`
	GraphSecretExpiry SecretExpiryInfo         `json:"graph_secret_expiry"`
	Version           VersionInfo              `json:"version"`
}

// APIConfigResponse contains the complete encoder configuration for API responses.
type APIConfigResponse struct {
	AudioInput string         `json:"audio_input"`
	Devices    []audio.Device `json:"devices"`
	Platform   string         `json:"platform"`

	SilenceThreshold  float64           `json:"silence_threshold"` // dB
	SilenceDurationMs int64             `json:"silence_duration_ms"`
	SilenceRecoveryMs int64             `json:"silence_recovery_ms"`
	SilenceDump       SilenceDumpConfig `json:"silence_dump"`

	WebhookURL string `json:"webhook_url"`

	ZabbixServer     string `json:"zabbix_server"`
	ZabbixPort       int    `json:"zabbix_port"`
	ZabbixHost       string `json:"zabbix_host"`
	ZabbixSilenceKey string `json:"zabbix_silence_key"`
	ZabbixUploadKey  string `json:"zabbix_upload_key"`

	GraphTenantID    string `json:"graph_tenant_id"`
	GraphClientID    string `json:"graph_client_id"`
	GraphFromAddress string `json:"graph_from_address"`
	GraphRecipients  string `json:"graph_recipients"` // Comma-separated
	GraphHasSecret   bool   `json:"graph_has_secret"`

	RecordingAPIKey string `json:"recording_api_key"`

	Streams   []Stream   `json:"streams"`
	Recorders []Recorder `json:"recorders"`
}

// WSLevelsResponse contains audio level data sent to clients.
type WSLevelsResponse struct {
	Type   string            `json:"type"` // Always "levels"
	Levels audio.AudioLevels `json:"levels"`
}

// GraphConfig holds credentials for Microsoft Graph email notifications.
type GraphConfig struct {
	TenantID     string `json:"tenant_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	FromAddress  string `json:"from_address,omitempty"`
	Recipients   string `json:"recipients,omitempty"` // Comma-separated
}

// ZabbixConfig holds settings for Zabbix trapper monitoring alerts.
type ZabbixConfig struct {
	Server     string `json:"server,omitempty"`
	Port       int    `json:"port,omitempty"` // Default 10051
	Host       string `json:"host,omitempty"`
	SilenceKey string `json:"silence_key,omitempty"`
	UploadKey  string `json:"upload_key,omitempty"`
}

// SecretExpiryInfo holds expiration details for an Azure client secret.
type SecretExpiryInfo struct {
	ExpiresAt   string `json:"expires_at,omitempty"` // RFC3339
	ExpiresSoon bool   `json:"expires_soon,omitempty"`
	DaysLeft    int    `json:"days_left,omitempty"`
	Error       string `json:"error,omitempty"`
}

// VersionInfo holds current and latest version details.
type VersionInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	UpdateAvail bool   `json:"update_available"`
	Commit      string `json:"commit,omitempty"`
	BuildTime   string `json:"build_time,omitempty"` // RFC3339
}
