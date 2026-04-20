// Package config provides application configuration management.
package config

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Sentinel errors for configuration operations.
var (
	// ErrStreamNotFound is returned when a stream ID does not exist in config.
	ErrStreamNotFound = errors.New("stream not found")

	// ErrRecorderNotFound is returned when a recorder ID does not exist in config.
	ErrRecorderNotFound = errors.New("recorder not found")
)

const (
	// DefaultWebPort is the default HTTP server port (8080).
	DefaultWebPort = 8080
	// DefaultZabbixPort is the default Zabbix trapper port.
	DefaultZabbixPort = 10051
	// DefaultWebUsername is the default web interface username (admin).
	DefaultWebUsername = "admin"
	// DefaultWebPassword is the default web interface password (encoder).
	DefaultWebPassword = "encoder"
	// DefaultSilenceThreshold is the default silence detection threshold (-40 dB).
	DefaultSilenceThreshold = -40.0
	// DefaultSilenceDurationMs is the default silence duration before alert (15 seconds).
	DefaultSilenceDurationMs = 15000
	// DefaultSilenceRecoveryMs is the default recovery duration before clearing alert (5 seconds).
	DefaultSilenceRecoveryMs = 5000
	// DefaultPeakHoldMs is the default VU meter peak hold duration (3 seconds).
	DefaultPeakHoldMs = 3000
	// DefaultStationName is the default station display name shown in the web UI.
	DefaultStationName = "ZuidWest FM"
	// DefaultStationColorLight is the default accent color for light theme (#E6007E).
	DefaultStationColorLight = "#E6007E"
	// DefaultStationColorDark is the default accent color for dark theme (#E6007E).
	DefaultStationColorDark = "#E6007E"
	// DefaultRecordingMaxDurationMinutes is the default max duration for on-demand recordings (4 hours).
	DefaultRecordingMaxDurationMinutes = 240
)

// SystemConfig holds system-level configuration.
type SystemConfig struct {
	// FFmpegPath is the path to the FFmpeg binary, or empty to search PATH.
	FFmpegPath string `json:"ffmpeg_path"`
	// Port is the HTTP server port to listen on.
	Port int `json:"port"`
	// Username is the web interface login username.
	Username string `json:"username"`
	// Password is the web interface login password.
	Password string `json:"password"` //nolint:gosec // G117: intentional config field for web UI auth
}

// WebConfig holds web UI branding settings.
type WebConfig struct {
	// StationName is the station display name shown in the web UI header.
	StationName string `json:"station_name"`
	// ColorLight is the accent color for light theme in hex format (#RRGGBB).
	ColorLight string `json:"color_light"`
	// ColorDark is the accent color for dark theme in hex format (#RRGGBB).
	ColorDark string `json:"color_dark"`
}

// AudioConfig holds audio input configuration.
type AudioConfig struct {
	// Input is the audio input device identifier (platform-specific).
	Input string `json:"input"`
}

// SilenceDetectionConfig holds silence detection settings.
type SilenceDetectionConfig struct {
	// ThresholdDB is the audio level in dB below which silence is detected.
	ThresholdDB float64 `json:"threshold_db"`
	// DurationMs is how long audio must be below threshold before alerting.
	DurationMs int64 `json:"duration_ms"`
	// RecoveryMs is how long audio must be above threshold before clearing the alert.
	RecoveryMs int64 `json:"recovery_ms"`
	// PeakHoldMs is how long the VU meter holds peak values before decay.
	// Config-file-only: not exposed in the settings API or web UI.
	// Adjust in config.json under silence_detection.peak_hold_ms (default: 3000 ms).
	PeakHoldMs int64 `json:"peak_hold_ms"`
}

// WebhookConfig holds webhook notification settings.
type WebhookConfig struct {
	// URL is the endpoint to POST silence alerts to.
	URL string `json:"url"`
	// Events controls which silence events trigger webhook notifications.
	Events types.EventSubscriptions `json:"events"`
}

// EmailConfig holds Microsoft Graph email settings.
type EmailConfig struct {
	// TenantID is the Azure AD tenant ID for Graph API authentication.
	TenantID string `json:"tenant_id"`
	// ClientID is the Azure app registration client ID.
	ClientID string `json:"client_id"`
	// ClientSecret is the Azure app registration client secret.
	ClientSecret string `json:"client_secret"` //nolint:gosec // G117: intentional config field for Azure auth
	// FromAddress is the shared mailbox address to send emails from.
	FromAddress string `json:"from_address"`
	// Recipients is a comma-separated list of email addresses to notify.
	Recipients string `json:"recipients"`
	// Events controls which silence events trigger email notifications.
	Events types.EventSubscriptions `json:"events"`
}

// NotificationsConfig holds notification settings.
type NotificationsConfig struct {
	// Webhook contains webhook notification settings.
	Webhook WebhookConfig `json:"webhook"`
	// Email contains Microsoft Graph email settings.
	Email EmailConfig `json:"email"`
	// Zabbix contains Zabbix notification settings.
	Zabbix types.ZabbixConfig `json:"zabbix"`
}

// StreamingConfig holds stream configuration.
type StreamingConfig struct {
	// Streams lists the configured stream destinations.
	Streams []types.Stream `json:"streams"`
}

// RecordingConfig holds recording configuration.
type RecordingConfig struct {
	// APIKey is the secret key for external recording control via REST API.
	APIKey string `json:"api_key"` //nolint:gosec // G117: intentional config field for recording API auth
	// MaxDurationMinutes is the maximum allowed duration for on-demand recordings.
	// Config-file-only: not exposed in the settings API or web UI.
	// Adjust in config.json under recording.max_duration_minutes (default: 240 min).
	MaxDurationMinutes int `json:"max_duration_minutes"`
	// Recorders lists configured recording destinations.
	Recorders []types.Recorder `json:"recorders"`
}

// Config holds all application configuration and is safe for concurrent use.
type Config struct {
	// System contains system-level configuration.
	System SystemConfig `json:"system"`
	// Web contains web UI branding settings.
	Web WebConfig `json:"web"`
	// Audio contains audio input settings.
	Audio AudioConfig `json:"audio"`
	// SilenceDetection contains silence detection settings.
	SilenceDetection SilenceDetectionConfig `json:"silence_detection"`
	// SilenceDump contains silence dump settings.
	SilenceDump types.SilenceDumpConfig `json:"silence_dump"`
	// Notifications contains notification settings.
	Notifications NotificationsConfig `json:"notifications"`
	// Streaming contains stream settings.
	Streaming StreamingConfig `json:"streaming"`
	// Recording contains recording settings.
	Recording RecordingConfig `json:"recording"`

	mu       sync.RWMutex
	filePath string
}

// New returns a Config with default values.
func New(filePath string) *Config {
	return &Config{
		System: SystemConfig{
			Port:     DefaultWebPort,
			Username: DefaultWebUsername,
			Password: DefaultWebPassword,
		},
		Web: WebConfig{
			StationName: DefaultStationName,
			ColorLight:  DefaultStationColorLight,
			ColorDark:   DefaultStationColorDark,
		},
		SilenceDetection: SilenceDetectionConfig{
			ThresholdDB: DefaultSilenceThreshold,
			DurationMs:  DefaultSilenceDurationMs,
			RecoveryMs:  DefaultSilenceRecoveryMs,
			PeakHoldMs:  DefaultPeakHoldMs,
		},
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       true, // Enabled by default when FFmpeg is available
			RetentionDays: types.DefaultSilenceDumpRetentionDays,
		},
		Notifications: NotificationsConfig{
			Webhook: WebhookConfig{
				Events: types.EventSubscriptions{SilenceStart: true, SilenceEnd: true, AudioDump: true},
			},
			Email: EmailConfig{
				Events: types.EventSubscriptions{SilenceStart: true, SilenceEnd: true, AudioDump: true},
			},
			Zabbix: types.ZabbixConfig{
				Port:   DefaultZabbixPort,
				Events: types.ZabbixEventSubscriptions{SilenceStart: true, SilenceEnd: true},
			},
		},
		Streaming: StreamingConfig{Streams: []types.Stream{}},
		Recording: RecordingConfig{
			MaxDurationMinutes: DefaultRecordingMaxDurationMinutes,
			Recorders:          []types.Recorder{},
		},
		filePath: filePath,
	}
}

// Load reads configuration from file or creates defaults.
func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.filePath)
	if os.IsNotExist(err) {
		if err := c.validate(); err != nil {
			return err
		}
		return c.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return util.WrapError("parse config", err)
	}

	if err := c.validate(); err != nil {
		return err
	}

	return nil
}

func (c *Config) validate() error {
	// Validate station name
	name := c.Web.StationName
	if name == "" || len(name) > 30 || !util.StationNamePattern.MatchString(name) {
		return fmt.Errorf("invalid station_name %q: must be 1-30 printable characters", name)
	}
	// Validate station colors
	if !util.StationColorPattern.MatchString(c.Web.ColorLight) {
		return fmt.Errorf("invalid color_light %q: must be hex format (#RRGGBB)", c.Web.ColorLight)
	}
	if !util.StationColorPattern.MatchString(c.Web.ColorDark) {
		return fmt.Errorf("invalid color_dark %q: must be hex format (#RRGGBB)", c.Web.ColorDark)
	}
	if msg := validateSilenceThreshold("silence_detection.threshold_db", c.SilenceDetection.ThresholdDB); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validatePositiveMilliseconds("silence_detection.duration_ms", c.SilenceDetection.DurationMs); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validatePositiveMilliseconds("silence_detection.recovery_ms", c.SilenceDetection.RecoveryMs); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validatePositiveMilliseconds("silence_detection.peak_hold_ms", c.SilenceDetection.PeakHoldMs); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validateNonNegativeDays("silence_dump.retention_days", c.SilenceDump.RetentionDays); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validateOptionalWebhookURL("notifications.webhook.url", c.Notifications.Webhook.URL); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validateOptionalEmail("notifications.email.from_address", c.Notifications.Email.FromAddress); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validateRecipients("notifications.email.recipients", c.Notifications.Email.Recipients); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	if msg := validateZabbixPort("notifications.zabbix.port", c.Notifications.Zabbix.Port); msg != "" {
		return fmt.Errorf("invalid %s", msg)
	}
	return nil
}

func validateSilenceThreshold(field string, threshold float64) string {
	if threshold > -1 || threshold < -60 {
		return field + ": must be between -60 and -1 dB"
	}
	return ""
}

func validatePositiveMilliseconds(field string, value int64) string {
	if value <= 0 {
		return field + ": must be greater than 0"
	}
	return ""
}

func validateNonNegativeDays(field string, value int) string {
	if value < 0 {
		return field + ": cannot be negative"
	}
	return ""
}

func validateOptionalWebhookURL(field, rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return field + ": invalid URL format"
	}
	return ""
}

func validateOptionalEmail(field, email string) string {
	if email != "" && !util.EmailPattern.MatchString(email) {
		return field + ": invalid email format"
	}
	return ""
}

func validateRecipients(field, recipients string) string {
	if recipients == "" {
		return ""
	}
	for email := range strings.SplitSeq(recipients, ",") {
		if trimmed := strings.TrimSpace(email); trimmed != "" && !util.EmailPattern.MatchString(trimmed) {
			return field + ": contains invalid email address"
		}
	}
	return ""
}

func validateZabbixPort(field string, port int) string {
	if port != 0 && (port < 1 || port > 65535) {
		return field + ": must be between 1 and 65535"
	}
	return ""
}

// saveLocked writes config to disk using a write-to-temp-then-rename strategy.
//
// On Linux and macOS, os.Rename within the same filesystem is atomic, so a
// crash mid-write leaves either the old or the new config intact — never a
// truncated file. The temp file is fsync'd before the rename, and the parent
// directory is synced after the rename on Unix, so the replacement is
// durable on those filesystems.
//
// On Windows, os.Rename (MoveFileExW) is not guaranteed to be atomic at the
// filesystem level, but it is still significantly safer than direct overwrite
// because the destination is only replaced after the source is fully written.
func (c *Config) saveLocked() (retErr error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return util.WrapError("marshal config", err)
	}

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Config directory needs to be readable
		return util.WrapError("create config directory", err)
	}

	// Write to a temp file in the same directory so the rename stays on the
	// same filesystem (required for atomic rename on Unix).
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return util.WrapError("create temp config", err)
	}

	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return util.WrapError("write temp config", err)
	}
	if err := tmp.Sync(); err != nil {
		return util.WrapError("sync temp config", err)
	}
	// Chmod while the fd is open so it applies via fchmod, avoiding a
	// symlink race between close and a path-based chmod call.
	if err := tmp.Chmod(0o600); err != nil {
		return util.WrapError("chmod temp config", err)
	}
	if err := tmp.Close(); err != nil {
		return util.WrapError("close temp config", err)
	}
	if err := os.Rename(tmpName, c.filePath); err != nil {
		return util.WrapError("rename config", err)
	}
	if err := syncDirPath(dir); err != nil {
		return err
	}

	return nil
}

// Stream management.

// ConfiguredStreams returns a copy of all streams.
func (c *Config) ConfiguredStreams() []types.Stream {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Streaming.Streams)
}

// Stream returns a copy of the stream with the given ID, or nil if not found.
func (c *Config) Stream(id string) *types.Stream {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := slices.IndexFunc(c.Streaming.Streams, func(s types.Stream) bool {
		return s.ID == id
	})
	if idx == -1 {
		return nil
	}
	s := c.Streaming.Streams[idx]
	return &s
}

func (c *Config) findStreamIndex(id string) int {
	return slices.IndexFunc(c.Streaming.Streams, func(s types.Stream) bool {
		return s.ID == id
	})
}

// AddStream adds a stream to the configuration and persists the change.
func (c *Config) AddStream(stream *types.Stream) error {
	if err := stream.Validate(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Generate unique ID
	shortID, err := generateShortID()
	if err != nil {
		return fmt.Errorf("failed to generate ID: %w", err)
	}
	stream.ID = fmt.Sprintf("stream-%s", shortID)

	// New streams are enabled by default
	stream.Enabled = true
	stream.CreatedAt = time.Now().UnixMilli()

	c.Streaming.Streams = append(c.Streaming.Streams, *stream)
	return c.saveLocked()
}

// RemoveStream removes a stream from the configuration and persists the change.
func (c *Config) RemoveStream(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findStreamIndex(id)
	if i == -1 {
		return fmt.Errorf("%w: %s", ErrStreamNotFound, id)
	}

	c.Streaming.Streams = slices.Delete(c.Streaming.Streams, i, i+1)
	return c.saveLocked()
}

// UpdateStream updates a stream in the configuration and persists the change.
func (c *Config) UpdateStream(stream *types.Stream) error {
	if err := stream.Validate(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findStreamIndex(stream.ID)
	if i == -1 {
		return fmt.Errorf("%w: %s", ErrStreamNotFound, stream.ID)
	}

	c.Streaming.Streams[i] = *stream
	return c.saveLocked()
}

// Recorder management.

// Recorder returns a copy of the recorder with the given ID, or nil if not found.
func (c *Config) Recorder(id string) *types.Recorder {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := slices.IndexFunc(c.Recording.Recorders, func(r types.Recorder) bool {
		return r.ID == id
	})
	if idx == -1 {
		return nil
	}
	r := c.Recording.Recorders[idx]
	return &r
}

func (c *Config) findRecorderIndex(id string) int {
	return slices.IndexFunc(c.Recording.Recorders, func(r types.Recorder) bool {
		return r.ID == id
	})
}

// AddRecorder adds a recorder to the configuration and persists the change.
func (c *Config) AddRecorder(recorder *types.Recorder) error {
	if err := recorder.Validate(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Generate unique ID
	shortID, err := generateShortID()
	if err != nil {
		return fmt.Errorf("failed to generate ID: %w", err)
	}
	recorder.ID = fmt.Sprintf("recorder-%s", shortID)

	// Apply retention days default if not specified
	recorder.RetentionDays = cmp.Or(recorder.RetentionDays, types.DefaultRetentionDays)
	// New recorders are enabled by default
	recorder.Enabled = true
	recorder.CreatedAt = time.Now().UnixMilli()

	c.Recording.Recorders = append(c.Recording.Recorders, *recorder)
	return c.saveLocked()
}

// RemoveRecorder removes a recorder from the configuration and persists the change.
func (c *Config) RemoveRecorder(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findRecorderIndex(id)
	if i == -1 {
		return fmt.Errorf("%w: %s", ErrRecorderNotFound, id)
	}

	c.Recording.Recorders = slices.Delete(c.Recording.Recorders, i, i+1)
	return c.saveLocked()
}

// UpdateRecorder updates a recorder in the configuration and persists the change.
func (c *Config) UpdateRecorder(recorder *types.Recorder) error {
	if err := recorder.Validate(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findRecorderIndex(recorder.ID)
	if i == -1 {
		return fmt.Errorf("%w: %s", ErrRecorderNotFound, recorder.ID)
	}

	c.Recording.Recorders[i] = *recorder
	return c.saveLocked()
}

// Getters for individual settings.

// AudioInput returns the configured audio input device.
func (c *Config) AudioInput() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Audio.Input
}

// FFmpegPath returns the configured FFmpeg binary path.
func (c *Config) FFmpegPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.System.FFmpegPath
}

// GraphConfig returns a copy of the current Graph/Email configuration.
func (c *Config) GraphConfig() types.GraphConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return types.GraphConfig{
		TenantID:     c.Notifications.Email.TenantID,
		ClientID:     c.Notifications.Email.ClientID,
		ClientSecret: c.Notifications.Email.ClientSecret,
		FromAddress:  c.Notifications.Email.FromAddress,
		Recipients:   c.Notifications.Email.Recipients,
	}
}

// RecordingAPIKey returns the API key for recording REST endpoints.
func (c *Config) RecordingAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Recording.APIKey
}

// Individual setters.

// SetRecordingAPIKey updates the recording API key and persists the change.
func (c *Config) SetRecordingAPIKey(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Recording.APIKey = key
	return c.saveLocked()
}

// Snapshot for atomic reads.

// Snapshot is a point-in-time copy of configuration values.
type Snapshot struct {
	// WebPort is the HTTP server port to listen on.
	WebPort int
	// WebUser is the web interface login username.
	WebUser string
	// WebPassword is the web interface login password.
	WebPassword string

	// StationName is the station display name shown in the web UI header.
	StationName string
	// StationColorLight is the accent color for light theme (#RRGGBB).
	StationColorLight string
	// StationColorDark is the accent color for dark theme (#RRGGBB).
	StationColorDark string

	// AudioInput is the audio input device identifier (platform-specific).
	AudioInput string

	// SilenceThreshold is the audio level in dB below which silence is detected.
	SilenceThreshold float64
	// SilenceDurationMs is how long audio must be below threshold before alerting.
	SilenceDurationMs int64
	// SilenceRecoveryMs is how long audio must be above threshold before clearing the alert.
	SilenceRecoveryMs int64
	// PeakHoldMs is how long the VU meter holds peak values before decay.
	// Config-file-only: not part of SettingsUpdate or the settings API.
	PeakHoldMs int64

	// SilenceDumpEnabled reports whether silence audio dumping is enabled.
	SilenceDumpEnabled bool
	// SilenceDumpRetentionDays is how many days to keep silence dump files.
	SilenceDumpRetentionDays int

	// WebhookURL is the endpoint to POST silence alerts to.
	WebhookURL string
	// WebhookEvents controls which silence events trigger webhook notifications.
	WebhookEvents types.EventSubscriptions
	// EmailEvents controls which silence events trigger email notifications.
	EmailEvents types.EventSubscriptions
	// ZabbixEvents controls which silence events trigger Zabbix notifications.
	// Internally this uses the unified event shape; AudioDump remains false for Zabbix.
	ZabbixEvents types.EventSubscriptions

	// ZabbixServer is the Zabbix trapper server hostname or IP.
	ZabbixServer string
	// ZabbixPort is the Zabbix trapper server port.
	ZabbixPort int
	// ZabbixHost is the host name as registered in Zabbix.
	ZabbixHost string
	// ZabbixSilenceKey is the item key for Zabbix silence trapper values.
	ZabbixSilenceKey string
	// ZabbixUploadKey is the item key for Zabbix upload trapper values.
	ZabbixUploadKey string

	// GraphTenantID is the Azure AD tenant ID for Graph API authentication.
	GraphTenantID string
	// GraphClientID is the Azure app registration client ID.
	GraphClientID string
	// GraphClientSecret is the Azure app registration client secret.
	GraphClientSecret string
	// GraphFromAddress is the shared mailbox address to send emails from.
	GraphFromAddress string
	// GraphRecipients is a comma-separated list of email addresses to notify.
	GraphRecipients string

	// RecordingAPIKey is the secret key for external recording control via REST API.
	RecordingAPIKey string
	// RecordingMaxDurationMinutes is the maximum allowed duration for on-demand recordings.
	// Config-file-only: not part of SettingsUpdate or the settings API.
	RecordingMaxDurationMinutes int

	// Streams lists configured stream destinations.
	Streams []types.Stream
	// Recorders lists configured recording destinations.
	Recorders []types.Recorder
}

// Snapshot returns a point-in-time copy of all configuration values.
func (c *Config) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Snapshot{
		// System
		WebPort:     c.System.Port,
		WebUser:     c.System.Username,
		WebPassword: c.System.Password,

		// Web/Branding
		StationName:       c.Web.StationName,
		StationColorLight: c.Web.ColorLight,
		StationColorDark:  c.Web.ColorDark,

		// Audio
		AudioInput: c.Audio.Input,

		// Silence Detection
		SilenceThreshold:  c.SilenceDetection.ThresholdDB,
		SilenceDurationMs: c.SilenceDetection.DurationMs,
		SilenceRecoveryMs: c.SilenceDetection.RecoveryMs,
		PeakHoldMs:        c.SilenceDetection.PeakHoldMs,

		// Silence Dump
		SilenceDumpEnabled:       c.SilenceDump.Enabled,
		SilenceDumpRetentionDays: c.SilenceDump.RetentionDays,

		// Notifications
		WebhookURL:    c.Notifications.Webhook.URL,
		WebhookEvents: c.Notifications.Webhook.Events,
		EmailEvents:   c.Notifications.Email.Events,
		ZabbixEvents:  c.Notifications.Zabbix.Events.ToEventSubscriptions(),

		// Zabbix
		ZabbixServer:     c.Notifications.Zabbix.Server,
		ZabbixPort:       c.Notifications.Zabbix.Port,
		ZabbixHost:       c.Notifications.Zabbix.Host,
		ZabbixSilenceKey: c.Notifications.Zabbix.SilenceKey,
		ZabbixUploadKey:  c.Notifications.Zabbix.UploadKey,

		// Microsoft Graph
		GraphTenantID:     c.Notifications.Email.TenantID,
		GraphClientID:     c.Notifications.Email.ClientID,
		GraphClientSecret: c.Notifications.Email.ClientSecret,
		GraphFromAddress:  c.Notifications.Email.FromAddress,
		GraphRecipients:   c.Notifications.Email.Recipients,

		// Recording
		RecordingAPIKey:             c.Recording.APIKey,
		RecordingMaxDurationMinutes: c.Recording.MaxDurationMinutes,

		// Entities
		Streams:   slices.Clone(c.Streaming.Streams),
		Recorders: slices.Clone(c.Recording.Recorders),
	}
}

// HasWebhook reports whether a webhook URL is configured.
func (s *Snapshot) HasWebhook() bool {
	return s.WebhookURL != ""
}

// HasGraph reports whether Microsoft Graph email notifications are configured.
func (s *Snapshot) HasGraph() bool {
	return s.GraphTenantID != "" && s.GraphClientID != "" && s.GraphClientSecret != "" &&
		s.GraphFromAddress != "" && s.GraphRecipients != ""
}

// HasZabbixSilence reports whether Zabbix silence alerting is configured.
func (s *Snapshot) HasZabbixSilence() bool {
	return s.ZabbixServer != "" && s.ZabbixHost != "" && s.ZabbixSilenceKey != ""
}

// HasZabbixUpload reports whether Zabbix upload alerting is configured.
func (s *Snapshot) HasZabbixUpload() bool {
	return s.ZabbixServer != "" && s.ZabbixHost != "" && s.ZabbixUploadKey != ""
}

// Atomic settings update.

// SettingsUpdate contains all settings for atomic update.
type SettingsUpdate struct {
	// AudioInput is the audio input device identifier (platform-specific).
	AudioInput string `json:"audio_input"`
	// SilenceThreshold is the audio level in dB below which silence is detected.
	SilenceThreshold float64 `json:"silence_threshold"`
	// SilenceDurationMs is how long audio must be below threshold before alerting.
	SilenceDurationMs int64 `json:"silence_duration_ms"`
	// SilenceRecoveryMs is how long audio must be above threshold before clearing the alert.
	SilenceRecoveryMs int64 `json:"silence_recovery_ms"`
	// SilenceDumpEnabled reports whether silence audio dumping is enabled.
	SilenceDumpEnabled bool `json:"silence_dump_enabled"`
	// SilenceDumpRetentionDays is how many days to keep silence dump files.
	SilenceDumpRetentionDays int `json:"silence_dump_retention_days"`
	// WebhookURL is the endpoint to POST silence alerts to.
	WebhookURL string `json:"webhook_url"`
	// WebhookEvents controls which silence events trigger webhook notifications.
	WebhookEvents types.EventSubscriptions `json:"webhook_events"`
	// EmailEvents controls which silence events trigger email notifications.
	EmailEvents types.EventSubscriptions `json:"email_events"`
	// ZabbixEvents controls which silence events trigger Zabbix notifications.
	// This stays on the public Zabbix-specific shape to avoid exposing audio_dump.
	ZabbixEvents types.ZabbixEventSubscriptions `json:"zabbix_events"`
	// ZabbixServer is the Zabbix trapper server hostname or IP.
	ZabbixServer string `json:"zabbix_server"`
	// ZabbixPort is the Zabbix trapper server port.
	ZabbixPort int `json:"zabbix_port"`
	// ZabbixHost is the host name as registered in Zabbix.
	ZabbixHost string `json:"zabbix_host"`
	// ZabbixSilenceKey is the item key for Zabbix silence trapper values.
	ZabbixSilenceKey string `json:"zabbix_silence_key"`
	// ZabbixUploadKey is the item key for Zabbix upload trapper values.
	ZabbixUploadKey string `json:"zabbix_upload_key"`
	// GraphTenantID is the Azure AD tenant ID for Graph API authentication.
	GraphTenantID string `json:"graph_tenant_id"`
	// GraphClientID is the Azure app registration client ID.
	GraphClientID string `json:"graph_client_id"`
	// GraphClientSecret is the Azure app registration client secret.
	GraphClientSecret string `json:"graph_client_secret"`
	// GraphFromAddress is the shared mailbox address to send emails from.
	GraphFromAddress string `json:"graph_from_address"`
	// GraphRecipients is a comma-separated list of email addresses to notify.
	GraphRecipients string `json:"graph_recipients"`
}

// Validate checks all settings fields and returns all validation errors.
//
//nolint:gocyclo // Validation functions naturally have many branches - one per field
func (s *SettingsUpdate) Validate() []string {
	var errs []string

	// Silence detection thresholds
	if msg := validateSilenceThreshold("silence_threshold", s.SilenceThreshold); msg != "" {
		errs = append(errs, msg)
	}
	if msg := validatePositiveMilliseconds("silence_duration_ms", s.SilenceDurationMs); msg != "" {
		errs = append(errs, msg)
	}
	if msg := validatePositiveMilliseconds("silence_recovery_ms", s.SilenceRecoveryMs); msg != "" {
		errs = append(errs, msg)
	}
	if msg := validateNonNegativeDays("silence_dump_retention_days", s.SilenceDumpRetentionDays); msg != "" {
		errs = append(errs, msg)
	}

	// Webhook URL format
	if msg := validateOptionalWebhookURL("webhook_url", s.WebhookURL); msg != "" {
		errs = append(errs, msg)
	}

	// Email address validation
	if msg := validateOptionalEmail("graph_from_address", s.GraphFromAddress); msg != "" {
		errs = append(errs, msg)
	}
	if msg := validateRecipients("graph_recipients", s.GraphRecipients); msg != "" {
		errs = append(errs, msg)
	}
	if msg := validateZabbixPort("zabbix_port", s.ZabbixPort); msg != "" {
		errs = append(errs, msg)
	}

	return errs
}

// ApplySettings updates all settings atomically with a single file write.
// Validation should be performed before calling this method.
func (c *Config) ApplySettings(s *SettingsUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Audio
	c.Audio.Input = s.AudioInput

	// Silence detection
	c.SilenceDetection.ThresholdDB = s.SilenceThreshold
	c.SilenceDetection.DurationMs = s.SilenceDurationMs
	c.SilenceDetection.RecoveryMs = s.SilenceRecoveryMs
	c.SilenceDump.Enabled = s.SilenceDumpEnabled
	c.SilenceDump.RetentionDays = s.SilenceDumpRetentionDays

	// Notifications
	c.Notifications.Webhook.URL = s.WebhookURL
	c.Notifications.Webhook.Events = s.WebhookEvents
	c.Notifications.Email.Events = s.EmailEvents
	c.Notifications.Zabbix.Events = s.ZabbixEvents
	c.Notifications.Zabbix.Server = s.ZabbixServer
	c.Notifications.Zabbix.Port = s.ZabbixPort
	c.Notifications.Zabbix.Host = s.ZabbixHost
	c.Notifications.Zabbix.SilenceKey = s.ZabbixSilenceKey
	c.Notifications.Zabbix.UploadKey = s.ZabbixUploadKey
	c.Notifications.Email.TenantID = s.GraphTenantID
	c.Notifications.Email.ClientID = s.GraphClientID
	c.Notifications.Email.ClientSecret = s.GraphClientSecret
	c.Notifications.Email.FromAddress = s.GraphFromAddress
	c.Notifications.Email.Recipients = s.GraphRecipients

	return c.saveLocked()
}

// Utility functions.

// GenerateAPIKey returns a new random API key.
func GenerateAPIKey() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 32
	result := make([]byte, length)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		result[i] = chars[n.Int64()]
	}
	return string(result), nil
}

func generateShortID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
