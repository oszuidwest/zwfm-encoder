// Package types defines shared types and constants used across the encoder.
package types

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
	State          ProcessState `json:"state"`
	Stable         bool         `json:"stable,omitempty"`
	Exhausted      bool         `json:"exhausted,omitempty"`
	RetryCount     int          `json:"retry_count,omitempty"`
	MaxRetries     int          `json:"max_retries,omitempty"`
	Error          string       `json:"error,omitempty"`
	Uptime         string       `json:"uptime,omitempty"`
	AudioDrops     int64        `json:"audio_drops,omitempty"`
	EncoderRunning bool         `json:"encoder_running,omitempty"`
	ClientCount    int64        `json:"client_count,omitempty"`
	// ListenerDrops counts chunks dropped from full fan-out subscriber queues.
	// It is emitted at 0 so clients can distinguish healthy from unavailable.
	ListenerDrops int64 `json:"listener_drops"`
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
// The zero value is invalid; persisted and API config must specify a codec.
type Codec string

const (
	// CodecPCM is uncompressed PCM in an MPEG-TS container.
	CodecPCM Codec = "pcm"
	// CodecMP3 is MPEG Audio Layer III.
	CodecMP3 Codec = "mp3"
	// CodecOpus is Opus (RFC 6716) in an MPEG-TS container.
	CodecOpus Codec = "opus"
)

// validCodecs is the set of supported audio codecs.
var validCodecs = map[Codec]bool{
	CodecPCM: true, CodecMP3: true, CodecOpus: true,
}

func validateCodec(codec Codec) error {
	if validCodecs[codec] {
		return nil
	}
	return fmt.Errorf("codec: must be pcm, mp3, or opus")
}

// UnmarshalJSON validates the codec value during JSON parsing.
func (c *Codec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	codec := Codec(s)
	if err := validateCodec(codec); err != nil {
		return err
	}
	*c = codec
	return nil
}

// StreamMode identifies whether an SRT stream connects out or listens locally.
type StreamMode string

const (
	// StreamModeCaller pushes audio to a remote SRT listener.
	StreamModeCaller StreamMode = "caller"
	// StreamModeListener starts a local SRT listener for clients to pull from.
	StreamModeListener StreamMode = "listener"
)

const (
	// DefaultListenerBindHost is the bind address used when a listener host is omitted.
	DefaultListenerBindHost = "0.0.0.0"
	minSRTPasswordLength    = 10
	maxSRTPasswordLength    = 64
)

// OrDefault returns caller when mode is empty for backwards-compatible legacy configs.
func (m StreamMode) OrDefault() StreamMode {
	if m == "" {
		return StreamModeCaller
	}
	return m
}

func validateStreamMode(mode StreamMode) error {
	switch mode.OrDefault() {
	case StreamModeCaller, StreamModeListener:
		return nil
	default:
		return fmt.Errorf("mode: must be caller or listener")
	}
}

// Stream defines an SRT streaming destination.
type Stream struct {
	ID         string     `json:"id"`
	Enabled    bool       `json:"enabled"`
	Mode       StreamMode `json:"mode"`
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	Password   string     `json:"password"` //nolint:gosec // G117: intentional config field for SRT stream auth
	StreamID   string     `json:"stream_id"`
	Codec      Codec      `json:"codec"`
	Bitrate    int        `json:"bitrate"`     // kbit/s, 0 = codec default
	MaxRetries int        `json:"max_retries"` // 0 = no retries
	CreatedAt  int64      `json:"created_at"`  // Unix ms
}

// DefaultMaxRetries is the default number of retry attempts for streams.
const DefaultMaxRetries = 99

// StreamRestartDelay is the delay between stopping and starting a stream during restart.
const StreamRestartDelay = 2000 * time.Millisecond

// ModeOrDefault returns the configured stream mode, defaulting legacy empty values to caller.
func (s *Stream) ModeOrDefault() StreamMode {
	if s == nil {
		return StreamModeCaller
	}
	return s.Mode.OrDefault()
}

// RequiresFFmpegSRT reports whether the stream depends on FFmpeg's SRT output.
// Caller-mode streams send SRT through FFmpeg; listener-mode streams serve SRT via the
// gosrt fanout, so FFmpeg's SRT capability is irrelevant to them.
func (s *Stream) RequiresFFmpegSRT() bool {
	return s.ModeOrDefault() != StreamModeListener
}

// ListenerBindHost returns the bind host for listener streams.
func (s *Stream) ListenerBindHost() string {
	host := strings.TrimSpace(s.Host)
	if host == "" {
		return DefaultListenerBindHost
	}
	return host
}

// Endpoint returns the host:port a stream uses: the remote address for caller
// streams, or the local bind address for listener streams.
func (s *Stream) Endpoint() string {
	if s.ModeOrDefault() == StreamModeListener {
		return fmt.Sprintf("%s:%d", s.ListenerBindHost(), s.Port)
	}
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// MaxRetriesOrDefault returns MaxRetries, or [DefaultMaxRetries] if not set.
func (s *Stream) MaxRetriesOrDefault() int {
	if s.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return s.MaxRetries
}

// codecPreset defines encoding parameters for a codec.
// A zero defaultBitrate omits -b:a for uncompressed codecs.
type codecPreset struct {
	encoder        string
	format         string
	defaultBitrate int // kbit/s; 0 omits -b:a
	extraArgs      []string
}

// codecPresets maps each codec to its FFmpeg encoding parameters.
var codecPresets = map[Codec]codecPreset{
	CodecMP3:  {encoder: "libmp3lame", format: "mp3", defaultBitrate: 320},
	CodecOpus: {encoder: "libopus", format: "mpegts", defaultBitrate: 128, extraArgs: []string{"-frame_duration", "10"}},
	// SMPTE 302M is the only PCM-in-MPEG-TS encoder Liquidsoap can decode.
	// FFmpeg marks s302m as experimental, so -strict -2 is required.
	CodecPCM: {encoder: "s302m", format: "mpegts", extraArgs: []string{"-strict", "-2"}},
}

// Format returns the output format for this codec.
func (c Codec) Format() string {
	if preset, ok := codecPresets[c]; ok {
		return preset.format
	}
	slog.Error("unknown codec format requested, falling back to PCM format", "codec", c)
	return codecPresets[CodecPCM].format
}

// DefaultBitrate returns the codec's default bitrate in kbit/s.
// It returns 0 for PCM and unknown codecs, meaning no -b:a flag.
func (c Codec) DefaultBitrate() int {
	return codecPresets[c].defaultBitrate
}

// BuildCodecArgs returns FFmpeg encoder arguments for the given codec and bitrate.
// A bitrate of 0 uses the codec's default settings.
func BuildCodecArgs(codec Codec, bitrate int) []string {
	preset, ok := codecPresets[codec]
	if !ok {
		slog.Error("unknown codec arguments requested, falling back to PCM encoder", "codec", codec)
		preset = codecPresets[CodecPCM]
	}
	args := []string{preset.encoder}
	if preset.defaultBitrate > 0 {
		kbit := preset.defaultBitrate
		if bitrate > 0 {
			kbit = bitrate
		}
		args = append(args, "-b:a", strconv.Itoa(kbit)+"k")
	}
	args = append(args, preset.extraArgs...)
	return args
}

// validateBitrate checks whether the bitrate is valid for the given codec.
// A bitrate of 0 is always valid (uses codec default). The codec itself is
// validated separately via validateCodec by each entity's Validate method.
func validateBitrate(codec Codec, bitrate int) error {
	if bitrate == 0 {
		return nil
	}
	switch codec {
	case CodecMP3:
		if bitrate < 64 || bitrate > 320 {
			return fmt.Errorf("bitrate: must be between 64 and 320 for MP3")
		}
	case CodecOpus:
		if bitrate < 64 || bitrate > 256 {
			return fmt.Errorf("bitrate: must be between 64 and 256 for Opus")
		}
	case CodecPCM:
		return fmt.Errorf("bitrate: not supported for PCM (uncompressed)")
	}
	return nil
}

// Validate reports an error if the stream configuration is invalid.
func (s *Stream) Validate() error {
	if err := validateStreamMode(s.Mode); err != nil {
		return err
	}
	mode := s.ModeOrDefault()
	if mode == StreamModeCaller && strings.TrimSpace(s.Host) == "" {
		return fmt.Errorf("host: is required")
	}
	if mode == StreamModeListener && strings.TrimSpace(s.StreamID) != "" {
		return fmt.Errorf("stream_id: not supported for listener mode")
	}
	if err := validateCodec(s.Codec); err != nil {
		return err
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("port: must be between 1 and 65535")
	}
	if s.MaxRetries < 0 {
		return fmt.Errorf("max_retries: cannot be negative")
	}
	if s.Password != "" && (len(s.Password) < minSRTPasswordLength || len(s.Password) > maxSRTPasswordLength) {
		return fmt.Errorf("password: must be empty or between 10 and 64 characters")
	}
	if err := validateBitrate(s.Codec, s.Bitrate); err != nil {
		return err
	}
	return nil
}

// RecordingMode defines how recordings are managed.
// The zero value is invalid; persisted and API config must specify a mode.
type RecordingMode string

const (
	// RecordingHourly rotates recordings at system clock hour boundaries.
	RecordingHourly RecordingMode = "hourly"
	// RecordingOnDemand allows manual start/stop via the API.
	RecordingOnDemand RecordingMode = "ondemand"
)

// validRecordingModes is the set of supported recording modes.
var validRecordingModes = map[RecordingMode]bool{
	RecordingHourly:   true,
	RecordingOnDemand: true,
}

func validateRecordingMode(mode RecordingMode) error {
	if validRecordingModes[mode] {
		return nil
	}
	return fmt.Errorf("recording_mode: must be hourly or ondemand")
}

// UnmarshalJSON validates the recording mode during JSON parsing.
func (m *RecordingMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	mode := RecordingMode(s)
	if err := validateRecordingMode(mode); err != nil {
		return err
	}
	*m = mode
	return nil
}

// StorageMode defines where recordings are stored.
// The zero value is invalid; persisted and API config must specify a mode.
type StorageMode string

const (
	// StorageLocal stores recordings on the local filesystem only.
	StorageLocal StorageMode = "local"
	// StorageS3 uploads recordings to S3-compatible storage only.
	StorageS3 StorageMode = "s3"
	// StorageBoth stores recordings locally and uploads to S3-compatible storage.
	StorageBoth StorageMode = "both"
)

// validStorageModes is the set of supported storage modes.
var validStorageModes = map[StorageMode]bool{
	StorageLocal: true, StorageS3: true, StorageBoth: true,
}

func validateStorageMode(mode StorageMode) error {
	if validStorageModes[mode] {
		return nil
	}
	return fmt.Errorf("storage_mode: must be local, s3, or both")
}

// UnmarshalJSON validates the storage mode during JSON parsing.
func (m *StorageMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	mode := StorageMode(s)
	if err := validateStorageMode(mode); err != nil {
		return err
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
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Enabled       bool          `json:"enabled"`
	Codec         Codec         `json:"codec"`
	Bitrate       int           `json:"bitrate"` // kbit/s, 0 = codec default
	RecordingMode RecordingMode `json:"recording_mode"`
	StorageMode   StorageMode   `json:"storage_mode"`
	LocalPath     string        `json:"local_path"`

	S3Endpoint        string `json:"s3_endpoint"`
	S3Bucket          string `json:"s3_bucket"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`

	RetentionDays int   `json:"retention_days"` // 0 = forever
	CreatedAt     int64 `json:"created_at"`     // Unix ms
}

// Validate reports an error if the recorder configuration is invalid.
func (r *Recorder) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name: is required")
	}
	if err := validateCodec(r.Codec); err != nil {
		return err
	}
	if err := validateRecordingMode(r.RecordingMode); err != nil {
		return err
	}
	if err := validateStorageMode(r.StorageMode); err != nil {
		return err
	}

	// Conditional validation based on storage mode
	needsLocal := r.StorageMode == StorageLocal || r.StorageMode == StorageBoth
	needsS3 := r.StorageMode == StorageS3 || r.StorageMode == StorageBoth

	if needsLocal && strings.TrimSpace(r.LocalPath) == "" {
		return fmt.Errorf("local_path: is required for local/both storage mode")
	}
	if needsS3 {
		for _, issue := range ValidateS3Credentials(r.S3Bucket, r.S3AccessKeyID, r.S3SecretAccessKey) {
			return fmt.Errorf("%s: is required for s3/both storage mode", issue.Field)
		}
	}
	if r.RetentionDays < 0 {
		return fmt.Errorf("retention_days: cannot be negative")
	}
	if err := validateBitrate(r.Codec, r.Bitrate); err != nil {
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
	Type               string                   `json:"type"` // Always "status"
	FFmpegAvailable    bool                     `json:"ffmpeg_available"`
	SRTAvailable       bool                     `json:"srt_available"`
	SRTError           string                   `json:"srt_error,omitempty"`
	RecordingAvailable bool                     `json:"recording_available"`
	Encoder            EncoderStatus            `json:"encoder"`
	StreamStatus       map[string]ProcessStatus `json:"stream_status"`
	RecorderStatuses   map[string]ProcessStatus `json:"recorder_statuses"`
	GraphSecretExpiry  SecretExpiryInfo         `json:"graph_secret_expiry"`
	Version            VersionInfo              `json:"version"`
	EventSeq           uint64                   `json:"event_seq"` // event-log change counter; clients refetch events only when it changes
}

// APIConfigResponse contains the complete encoder configuration for API responses.
type APIConfigResponse struct {
	AudioInput string         `json:"audio_input"`
	Devices    []audio.Device `json:"devices"`
	Platform   string         `json:"platform"`

	SilenceThreshold  float64           `json:"silence_threshold"` // dB
	SilenceDurationMs int64             `json:"silence_duration_ms"`
	SilenceRecoveryMs int64             `json:"silence_recovery_ms"`
	PeakHoldMs        int64             `json:"peak_hold_ms"`
	SilenceDump       SilenceDumpConfig `json:"silence_dump"`

	ChannelImbalanceThreshold  float64 `json:"channel_imbalance_threshold"` // dB
	ChannelImbalanceDurationMs int64   `json:"channel_imbalance_duration_ms"`
	ChannelImbalanceRecoveryMs int64   `json:"channel_imbalance_recovery_ms"`

	WebhookHasURL bool               `json:"webhook_has_url"`
	WebhookEvents EventSubscriptions `json:"webhook_events"`

	ZabbixServer     string                   `json:"zabbix_server"`
	ZabbixPort       int                      `json:"zabbix_port"`
	ZabbixHost       string                   `json:"zabbix_host"`
	ZabbixSilenceKey string                   `json:"zabbix_silence_key"`
	ZabbixUploadKey  string                   `json:"zabbix_upload_key"`
	ZabbixEvents     ZabbixEventSubscriptions `json:"zabbix_events"`

	GraphTenantID    string             `json:"graph_tenant_id"`
	GraphClientID    string             `json:"graph_client_id"`
	GraphFromAddress string             `json:"graph_from_address"`
	GraphRecipients  string             `json:"graph_recipients"` // Comma-separated
	GraphHasSecret   bool               `json:"graph_has_secret"`
	EmailEvents      EventSubscriptions `json:"email_events"`

	RecordingHasAPIKey          bool   `json:"recording_has_api_key"`
	RecordingMaxDurationMinutes int    `json:"recording_max_duration_minutes"`
	SRTAvailable                bool   `json:"srt_available"`
	SRTError                    string `json:"srt_error,omitempty"`

	Streams   []StreamResponse   `json:"streams"`
	Recorders []RecorderResponse `json:"recorders"`
}

// StreamResponse is the browser-safe representation of a stream.
type StreamResponse struct {
	ID          string     `json:"id"`
	Enabled     bool       `json:"enabled"`
	Mode        StreamMode `json:"mode"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	HasPassword bool       `json:"has_password"`
	StreamID    string     `json:"stream_id"`
	Codec       Codec      `json:"codec"`
	Bitrate     int        `json:"bitrate"`     // kbit/s, 0 = codec default
	MaxRetries  int        `json:"max_retries"` // 0 = no retries
	CreatedAt   int64      `json:"created_at"`  // Unix ms
}

// RecorderResponse is the browser-safe representation of a recorder.
type RecorderResponse struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Enabled       bool          `json:"enabled"`
	Codec         Codec         `json:"codec"`
	Bitrate       int           `json:"bitrate"` // kbit/s, 0 = codec default
	RecordingMode RecordingMode `json:"recording_mode"`
	StorageMode   StorageMode   `json:"storage_mode"`
	LocalPath     string        `json:"local_path"`

	S3Endpoint    string `json:"s3_endpoint"`
	S3Bucket      string `json:"s3_bucket"`
	S3AccessKeyID string `json:"s3_access_key_id"`
	HasS3Secret   bool   `json:"has_s3_secret"`

	RetentionDays int   `json:"retention_days"` // 0 = forever
	CreatedAt     int64 `json:"created_at"`     // Unix ms
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
	ClientSecret string `json:"client_secret,omitempty"` //nolint:gosec // G117: intentional config field for Azure auth
	FromAddress  string `json:"from_address,omitempty"`
	Recipients   string `json:"recipients,omitempty"` // Comma-separated
}

// EventSubscriptions controls which silence events a notification channel receives.
type EventSubscriptions struct {
	// SilenceStart enables notifications when silence is first detected.
	SilenceStart bool `json:"silence_start"`
	// SilenceEnd enables notifications when audio recovers from silence.
	SilenceEnd bool `json:"silence_end"`
	// AudioDump enables notifications when the audio dump MP3 is ready.
	AudioDump bool `json:"audio_dump"`
}

// ZabbixEventSubscriptions controls which silence events trigger Zabbix notifications.
// Zabbix trapper items carry only numeric values and do not support file attachments,
// so AudioDump is not available for this channel.
type ZabbixEventSubscriptions struct {
	// SilenceStart enables notifications when silence is first detected.
	SilenceStart bool `json:"silence_start"`
	// SilenceEnd enables notifications when audio recovers from silence.
	SilenceEnd bool `json:"silence_end"`
}

// ToEventSubscriptions converts the public Zabbix event shape to the internal unified event shape.
func (s ZabbixEventSubscriptions) ToEventSubscriptions() EventSubscriptions {
	return EventSubscriptions{
		SilenceStart: s.SilenceStart,
		SilenceEnd:   s.SilenceEnd,
	}
}

// ToZabbixEventSubscriptions narrows the internal unified event shape to the public Zabbix event shape.
// AudioDump is deliberately omitted: Zabbix trapper items cannot carry file attachments.
func (s EventSubscriptions) ToZabbixEventSubscriptions() ZabbixEventSubscriptions {
	return ZabbixEventSubscriptions{
		SilenceStart: s.SilenceStart,
		SilenceEnd:   s.SilenceEnd,
	}
}

// ZabbixConfig holds settings for Zabbix trapper monitoring alerts.
type ZabbixConfig struct {
	Server     string `json:"server,omitempty"`
	Port       int    `json:"port,omitempty"` // Default 10051
	Host       string `json:"host,omitempty"`
	SilenceKey string `json:"silence_key,omitempty"`
	UploadKey  string `json:"upload_key,omitempty"`
	// Events controls which silence events trigger Zabbix notifications.
	// Value type (not pointer): JSON null and missing field both leave the
	// preloaded default intact. To disable all events, set silence_start and
	// silence_end to false explicitly.
	Events ZabbixEventSubscriptions `json:"events"`
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
