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
	// DefaultWebPort is the default HTTP server port.
	DefaultWebPort = 8080
	// DefaultWebUsername is the default web username.
	DefaultWebUsername = "admin"
	// DefaultWebPassword is the default web password.
	DefaultWebPassword = "encoder"
	// DefaultSilenceThreshold is the default silence threshold in dB.
	DefaultSilenceThreshold = -40.0
	// DefaultSilenceDurationMs is the default silence duration in milliseconds.
	DefaultSilenceDurationMs = 15000
	// DefaultSilenceRecoveryMs is the default recovery duration in milliseconds.
	DefaultSilenceRecoveryMs = 5000
	// DefaultPeakHoldMs is the default peak hold duration in milliseconds.
	DefaultPeakHoldMs = 3000
	// DefaultStationName is the default station name.
	DefaultStationName = "ZuidWest FM"
	// DefaultStationColorLight is the default light theme color.
	DefaultStationColorLight = "#E6007E"
	// DefaultStationColorDark is the default dark theme color.
	DefaultStationColorDark = "#E6007E"
	// DefaultRecordingMaxDurationMinutes is the default max duration for on-demand recorders in minutes.
	DefaultRecordingMaxDurationMinutes = 240
)

// SystemConfig holds system-level configuration.
type SystemConfig struct {
	// FFmpegPath is the path to the FFmpeg binary (empty uses PATH).
	FFmpegPath string `json:"ffmpeg_path"`
	// Port is the HTTP server port.
	Port int `json:"port"`
	// Username is the login username.
	Username string `json:"username"`
	// Password is the login password.
	Password string `json:"password"`
}

// WebConfig holds web UI branding settings.
type WebConfig struct {
	// StationName is the station display name.
	StationName string `json:"station_name"`
	// ColorLight is the light theme color (#RRGGBB).
	ColorLight string `json:"color_light"`
	// ColorDark is the dark theme color (#RRGGBB).
	ColorDark string `json:"color_dark"`
}

// AudioConfig holds audio input configuration.
type AudioConfig struct {
	// Input is the audio input device identifier.
	Input string `json:"input"`
}

// SilenceDetectionConfig holds silence detection settings.
type SilenceDetectionConfig struct {
	// ThresholdDB is the silence threshold in dB.
	ThresholdDB float64 `json:"threshold_db"`
	// DurationMs is the duration below threshold before silence alert.
	DurationMs int64 `json:"duration_ms"`
	// RecoveryMs is the duration above threshold before recovery.
	RecoveryMs int64 `json:"recovery_ms"`
	// PeakHoldMs is the duration to hold peak values in the VU meter.
	PeakHoldMs int64 `json:"peak_hold_ms"`
}

// WebhookConfig holds webhook notification settings.
type WebhookConfig struct {
	// URL is the webhook URL for silence alerts.
	URL string `json:"url"`
}

// EmailConfig holds Microsoft Graph email settings.
type EmailConfig struct {
	// TenantID is the Azure AD tenant ID.
	TenantID string `json:"tenant_id"`
	// ClientID is the app registration client ID.
	ClientID string `json:"client_id"`
	// ClientSecret is the app registration client secret.
	ClientSecret string `json:"client_secret"`
	// FromAddress is the shared mailbox sender address.
	FromAddress string `json:"from_address"`
	// Recipients is a comma-separated list of recipient addresses.
	Recipients string `json:"recipients"`
}

// NotificationsConfig holds notification settings.
type NotificationsConfig struct {
	// Webhook contains webhook notification settings.
	Webhook WebhookConfig `json:"webhook"`
	// Email contains Microsoft Graph email settings.
	Email EmailConfig `json:"email"`
	// Zabbix contains Zabbix notification settings.
	Zabbix types.ZabbixConfig `json:"zabbix,omitempty"`
}

// StreamingConfig holds stream configuration.
type StreamingConfig struct {
	// Streams lists the configured stream destinations.
	Streams []types.Stream `json:"streams"`
}

// RecordingConfig holds recording configuration.
type RecordingConfig struct {
	// APIKey is the API key for recording control.
	APIKey string `json:"api_key"`
	// MaxDurationMinutes is the max duration for on-demand recorders.
	MaxDurationMinutes int `json:"max_duration_minutes"`
	// Recorders lists configured recording destinations.
	Recorders []types.Recorder `json:"recorders"`
}

// Config is safe for concurrent use.
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
		Audio:            AudioConfig{},
		SilenceDetection: SilenceDetectionConfig{},
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       true, // Enabled by default when FFmpeg is available
			RetentionDays: types.DefaultSilenceDumpRetentionDays,
		},
		Notifications: NotificationsConfig{},
		Streaming:     StreamingConfig{Streams: []types.Stream{}},
		Recording:     RecordingConfig{Recorders: []types.Recorder{}},
		filePath:      filePath,
	}
}

// Load reads configuration from file or creates defaults.
func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.filePath)
	if os.IsNotExist(err) {
		return c.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return util.WrapError("parse config", err)
	}

	c.applyDefaults()

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
	return nil
}

func (c *Config) applyDefaults() {
	// System defaults
	c.System.Port = cmp.Or(c.System.Port, DefaultWebPort)
	c.System.Username = cmp.Or(c.System.Username, DefaultWebUsername)
	c.System.Password = cmp.Or(c.System.Password, DefaultWebPassword)
	// Web defaults
	c.Web.StationName = cmp.Or(c.Web.StationName, DefaultStationName)
	c.Web.ColorLight = cmp.Or(c.Web.ColorLight, DefaultStationColorLight)
	c.Web.ColorDark = cmp.Or(c.Web.ColorDark, DefaultStationColorDark)
	// Silence detection defaults
	c.SilenceDetection.ThresholdDB = cmp.Or(c.SilenceDetection.ThresholdDB, DefaultSilenceThreshold)
	c.SilenceDetection.DurationMs = cmp.Or(c.SilenceDetection.DurationMs, DefaultSilenceDurationMs)
	c.SilenceDetection.RecoveryMs = cmp.Or(c.SilenceDetection.RecoveryMs, DefaultSilenceRecoveryMs)
	c.SilenceDetection.PeakHoldMs = cmp.Or(c.SilenceDetection.PeakHoldMs, DefaultPeakHoldMs)
	// Streaming defaults
	if c.Streaming.Streams == nil {
		c.Streaming.Streams = []types.Stream{}
	}
	for i := range c.Streaming.Streams {
		if c.Streaming.Streams[i].CreatedAt == 0 {
			c.Streaming.Streams[i].CreatedAt = time.Now().UnixMilli()
		}
	}
	// Recording defaults
	if c.Recording.Recorders == nil {
		c.Recording.Recorders = []types.Recorder{}
	}
	for i := range c.Recording.Recorders {
		if c.Recording.Recorders[i].CreatedAt == 0 {
			c.Recording.Recorders[i].CreatedAt = time.Now().UnixMilli()
		}
	}
}

func (c *Config) saveLocked() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return util.WrapError("marshal config", err)
	}

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return util.WrapError("create config directory", err)
	}

	if err := os.WriteFile(c.filePath, data, 0o600); err != nil {
		return util.WrapError("write config", err)
	}

	return nil
}

// --- Stream management ---

// ConfiguredStreams returns a copy of all streams.
func (c *Config) ConfiguredStreams() []types.Stream {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Streaming.Streams)
}

// Stream returns the stream with the given ID, or nil if not found.
func (c *Config) Stream(id string) *types.Stream {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := slices.IndexFunc(c.Streaming.Streams, func(s types.Stream) bool {
		return s.ID == id
	})
	if idx == -1 {
		return nil
	}
	return &c.Streaming.Streams[idx]
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

// --- Recorder management ---

// Recorder returns the recorder with the given ID, or nil if not found.
func (c *Config) Recorder(id string) *types.Recorder {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := slices.IndexFunc(c.Recording.Recorders, func(r types.Recorder) bool {
		return r.ID == id
	})
	if idx == -1 {
		return nil
	}
	return &c.Recording.Recorders[idx]
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

// --- Getters for individual settings ---

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

// --- Individual Setters ---

// SetRecordingAPIKey updates the recording API key and persists the change.
func (c *Config) SetRecordingAPIKey(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Recording.APIKey = key
	return c.saveLocked()
}

// --- Snapshot for atomic reads ---

// Snapshot is a point-in-time copy of configuration values.
type Snapshot struct {
	// WebPort is the configured HTTP server port.
	WebPort int
	// WebUser is the configured web username.
	WebUser string
	// WebPassword is the configured web password.
	WebPassword string

	// StationName is the station display name.
	StationName string
	// StationColorLight is the light theme color (#RRGGBB).
	StationColorLight string
	// StationColorDark is the dark theme color (#RRGGBB).
	StationColorDark string

	// AudioInput is the configured audio input identifier.
	AudioInput string

	// SilenceThreshold is the silence threshold in dB.
	SilenceThreshold float64
	// SilenceDurationMs is the silence duration threshold in milliseconds.
	SilenceDurationMs int64
	// SilenceRecoveryMs is the silence recovery duration in milliseconds.
	SilenceRecoveryMs int64
	// PeakHoldMs is the peak hold duration in milliseconds.
	PeakHoldMs int64

	// SilenceDumpEnabled reports whether silence dumping is enabled.
	SilenceDumpEnabled bool
	// SilenceDumpRetentionDays is the silence dump retention period in days.
	SilenceDumpRetentionDays int

	// WebhookURL is the configured webhook URL.
	WebhookURL string

	// ZabbixServer is the configured Zabbix server host.
	ZabbixServer string
	// ZabbixPort is the configured Zabbix server port.
	ZabbixPort int
	// ZabbixHost is the configured Zabbix host name.
	ZabbixHost string
	// ZabbixKey is the configured Zabbix item key.
	ZabbixKey string

	// GraphTenantID is the Microsoft Graph tenant ID.
	GraphTenantID string
	// GraphClientID is the Microsoft Graph client ID.
	GraphClientID string
	// GraphClientSecret is the Microsoft Graph client secret.
	GraphClientSecret string
	// GraphFromAddress is the sender email address.
	GraphFromAddress string
	// GraphRecipients is a comma-separated recipient list.
	GraphRecipients string

	// RecordingAPIKey is the API key for recording endpoints.
	RecordingAPIKey string
	// RecordingMaxDurationMinutes is the max duration for on-demand recorders.
	RecordingMaxDurationMinutes int

	// Streams lists configured stream destinations.
	Streams []types.Stream
	// Recorders lists configured recorders.
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

		// Silence Detection (with defaults)
		SilenceThreshold:  cmp.Or(c.SilenceDetection.ThresholdDB, DefaultSilenceThreshold),
		SilenceDurationMs: cmp.Or(c.SilenceDetection.DurationMs, DefaultSilenceDurationMs),
		SilenceRecoveryMs: cmp.Or(c.SilenceDetection.RecoveryMs, DefaultSilenceRecoveryMs),
		PeakHoldMs:        cmp.Or(c.SilenceDetection.PeakHoldMs, DefaultPeakHoldMs),

		// Silence Dump
		SilenceDumpEnabled:       c.SilenceDump.Enabled,
		SilenceDumpRetentionDays: cmp.Or(c.SilenceDump.RetentionDays, types.DefaultSilenceDumpRetentionDays),

		// Notifications
		WebhookURL: c.Notifications.Webhook.URL,

		// Zabbix
		ZabbixServer: c.Notifications.Zabbix.Server,
		ZabbixPort:   cmp.Or(c.Notifications.Zabbix.Port, 10051),
		ZabbixHost:   c.Notifications.Zabbix.Host,
		ZabbixKey:    c.Notifications.Zabbix.Key,

		// Microsoft Graph
		GraphTenantID:     c.Notifications.Email.TenantID,
		GraphClientID:     c.Notifications.Email.ClientID,
		GraphClientSecret: c.Notifications.Email.ClientSecret,
		GraphFromAddress:  c.Notifications.Email.FromAddress,
		GraphRecipients:   c.Notifications.Email.Recipients,

		// Recording
		RecordingAPIKey:             c.Recording.APIKey,
		RecordingMaxDurationMinutes: cmp.Or(c.Recording.MaxDurationMinutes, DefaultRecordingMaxDurationMinutes),

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

// HasZabbix reports whether Zabbix settings are configured.
func (s *Snapshot) HasZabbix() bool {
	return s.ZabbixServer != "" && s.ZabbixHost != "" && s.ZabbixKey != ""
}

// --- Atomic Settings Update ---

// SettingsUpdate contains all settings for atomic update.
type SettingsUpdate struct {
	// AudioInput is the audio input identifier.
	AudioInput string `json:"audio_input"`
	// SilenceThreshold is the silence threshold in dB.
	SilenceThreshold float64 `json:"silence_threshold"`
	// SilenceDurationMs is the silence duration in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms"`
	// SilenceRecoveryMs is the silence recovery duration in milliseconds.
	SilenceRecoveryMs int64 `json:"silence_recovery_ms"`
	// SilenceDumpEnabled reports whether silence dumps are enabled.
	SilenceDumpEnabled bool `json:"silence_dump_enabled"`
	// SilenceDumpRetentionDays is the retention period in days.
	SilenceDumpRetentionDays int `json:"silence_dump_retention_days"`
	// WebhookURL is the webhook URL for notifications.
	WebhookURL string `json:"webhook_url"`
	// ZabbixServer is the Zabbix server host.
	ZabbixServer string `json:"zabbix_server"`
	// ZabbixPort is the Zabbix server port.
	ZabbixPort int `json:"zabbix_port"`
	// ZabbixHost is the Zabbix host name.
	ZabbixHost string `json:"zabbix_host"`
	// ZabbixKey is the Zabbix item key.
	ZabbixKey string `json:"zabbix_key"`
	// GraphTenantID is the Microsoft Graph tenant ID.
	GraphTenantID string `json:"graph_tenant_id"`
	// GraphClientID is the Microsoft Graph client ID.
	GraphClientID string `json:"graph_client_id"`
	// GraphClientSecret is the Microsoft Graph client secret.
	GraphClientSecret string `json:"graph_client_secret"`
	// GraphFromAddress is the sender email address.
	GraphFromAddress string `json:"graph_from_address"`
	// GraphRecipients is a comma-separated recipient list.
	GraphRecipients string `json:"graph_recipients"`
}

// Validate checks all settings fields and returns all validation errors.
//
//nolint:gocyclo // Validation functions naturally have many branches - one per field
func (s *SettingsUpdate) Validate() []string {
	var errs []string

	// Silence detection thresholds
	if s.SilenceThreshold > 0 || s.SilenceThreshold < -60 {
		errs = append(errs, "silence_threshold: must be between -60 and 0 dB")
	}
	if s.SilenceDurationMs <= 0 {
		errs = append(errs, "silence_duration_ms: must be greater than 0")
	}
	if s.SilenceRecoveryMs <= 0 {
		errs = append(errs, "silence_recovery_ms: must be greater than 0")
	}
	if s.SilenceDumpRetentionDays < 0 {
		errs = append(errs, "silence_dump_retention_days: cannot be negative")
	}

	// Webhook URL format
	if s.WebhookURL != "" {
		if _, err := url.ParseRequestURI(s.WebhookURL); err != nil {
			errs = append(errs, "webhook_url: invalid URL format")
		}
	}

	// Email address validation
	if s.GraphFromAddress != "" && !util.EmailPattern.MatchString(s.GraphFromAddress) {
		errs = append(errs, "graph_from_address: invalid email format")
	}
	if s.GraphRecipients != "" {
		for _, email := range strings.Split(s.GraphRecipients, ",") {
			if trimmed := strings.TrimSpace(email); trimmed != "" && !util.EmailPattern.MatchString(trimmed) {
				errs = append(errs, "graph_recipients: contains invalid email address")
				break
			}
		}
	}

	// Zabbix port range
	if s.ZabbixPort != 0 && (s.ZabbixPort < 1 || s.ZabbixPort > 65535) {
		errs = append(errs, "zabbix_port: must be between 1 and 65535")
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
	c.Notifications.Zabbix.Server = s.ZabbixServer
	c.Notifications.Zabbix.Port = s.ZabbixPort
	c.Notifications.Zabbix.Host = s.ZabbixHost
	c.Notifications.Zabbix.Key = s.ZabbixKey
	c.Notifications.Email.TenantID = s.GraphTenantID
	c.Notifications.Email.ClientID = s.GraphClientID
	c.Notifications.Email.ClientSecret = s.GraphClientSecret
	c.Notifications.Email.FromAddress = s.GraphFromAddress
	c.Notifications.Email.Recipients = s.GraphRecipients

	return c.saveLocked()
}

// --- Utility functions ---

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
