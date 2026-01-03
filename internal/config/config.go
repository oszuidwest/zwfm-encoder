// Package config provides application configuration management.
package config

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Configuration defaults are used when values are not specified.
const (
	DefaultWebPort                     = 8080
	DefaultWebUsername                 = "admin"
	DefaultWebPassword                 = "encoder"
	DefaultSilenceThreshold            = -40.0
	DefaultSilenceDurationMs           = 15000 // 15 seconds in milliseconds
	DefaultSilenceRecoveryMs           = 5000  // 5 seconds in milliseconds
	DefaultStationName                 = "ZuidWest FM"
	DefaultStationColorLight           = "#E6007E"
	DefaultStationColorDark            = "#E6007E"
	DefaultRecordingMaxDurationMinutes = 240 // 4 hours for on-demand recorders
)

// Validation patterns define regular expressions for configuration value validation.
var (
	// Station name: any printable characters except control chars (blocks CRLF injection in emails)
	stationNamePattern  = regexp.MustCompile(`^[^\x00-\x1F\x7F]+$`)
	stationColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// SystemConfig holds system-level settings that require restart.
type SystemConfig struct {
	FFmpegPath string `json:"ffmpeg_path"` // Path to FFmpeg binary (empty = use PATH)
	Port       int    `json:"port"`        // HTTP server port
	Username   string `json:"username"`    // Login username
	Password   string `json:"password"`    // Login password
}

// WebConfig holds station branding settings.
type WebConfig struct {
	StationName string `json:"station_name"` // Station display name
	ColorLight  string `json:"color_light"`  // Theme color for light mode (#RRGGBB)
	ColorDark   string `json:"color_dark"`   // Theme color for dark mode (#RRGGBB)
}

// AudioConfig holds audio input device settings.
type AudioConfig struct {
	Input string `json:"input"` // Audio input device identifier
}

// SilenceDetectionConfig holds silence detection thresholds and timing parameters.
type SilenceDetectionConfig struct {
	ThresholdDB float64 `json:"threshold_db"` // Silence threshold in dB
	DurationMs  int64   `json:"duration_ms"`  // Duration below threshold before silence alert
	RecoveryMs  int64   `json:"recovery_ms"`  // Duration above threshold before recovery
}

// WebhookConfig holds webhook notification settings.
type WebhookConfig struct {
	URL string `json:"url"` // Webhook URL for silence alerts
}

// LogConfig holds log file notification settings.
type LogConfig struct {
	Path string `json:"path"` // Log file path for silence events
}

// EmailConfig holds Microsoft Graph email notification settings.
type EmailConfig struct {
	TenantID     string `json:"tenant_id"`     // Azure AD tenant ID
	ClientID     string `json:"client_id"`     // App registration client ID
	ClientSecret string `json:"client_secret"` // App registration client secret
	FromAddress  string `json:"from_address"`  // Shared mailbox sender address
	Recipients   string `json:"recipients"`    // Comma-separated recipient addresses
}

// NotificationsConfig holds all notification channel settings.
type NotificationsConfig struct {
	Webhook WebhookConfig `json:"webhook"` // Webhook settings
	Log     LogConfig     `json:"log"`     // Log file settings
	Email   EmailConfig   `json:"email"`   // Email settings
}

// StreamingConfig holds SRT output streaming settings.
type StreamingConfig struct {
	Outputs []types.Output `json:"outputs"` // SRT output destinations
}

// RecordingConfig holds recording settings.
type RecordingConfig struct {
	APIKey             string           `json:"api_key"`              // API key for recording control
	MaxDurationMinutes int              `json:"max_duration_minutes"` // Max duration for on-demand recorders
	Recorders          []types.Recorder `json:"recorders"`            // Recording destinations
}

// Config holds all application configuration. It is safe for concurrent use.
type Config struct {
	System           SystemConfig           `json:"system"`
	Web              WebConfig              `json:"web"`
	Audio            AudioConfig            `json:"audio"`
	SilenceDetection SilenceDetectionConfig `json:"silence_detection"`
	Notifications    NotificationsConfig    `json:"notifications"`
	Streaming        StreamingConfig        `json:"streaming"`
	Recording        RecordingConfig        `json:"recording"`

	mu       sync.RWMutex
	filePath string
}

// New creates a new Config with default values.
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
		Notifications:    NotificationsConfig{},
		Streaming:        StreamingConfig{Outputs: []types.Output{}},
		Recording:        RecordingConfig{Recorders: []types.Recorder{}},
		filePath:         filePath,
	}
}

// Load reads config from file, creating a default if none exists.
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

// validate checks all configuration fields for correctness.
func (c *Config) validate() error {
	// Validate station name
	name := c.Web.StationName
	if name == "" || len(name) > 30 || !stationNamePattern.MatchString(name) {
		return fmt.Errorf("invalid station_name %q: must be 1-30 printable characters", name)
	}
	// Validate station colors
	if !stationColorPattern.MatchString(c.Web.ColorLight) {
		return fmt.Errorf("invalid color_light %q: must be hex format (#RRGGBB)", c.Web.ColorLight)
	}
	if !stationColorPattern.MatchString(c.Web.ColorDark) {
		return fmt.Errorf("invalid color_dark %q: must be hex format (#RRGGBB)", c.Web.ColorDark)
	}
	return nil
}

// applyDefaults sets default values for zero-value fields.
func (c *Config) applyDefaults() {
	// System defaults
	if c.System.Port == 0 {
		c.System.Port = DefaultWebPort
	}
	if c.System.Username == "" {
		c.System.Username = DefaultWebUsername
	}
	if c.System.Password == "" {
		c.System.Password = DefaultWebPassword
	}
	// Web defaults
	if c.Web.StationName == "" {
		c.Web.StationName = DefaultStationName
	}
	if c.Web.ColorLight == "" {
		c.Web.ColorLight = DefaultStationColorLight
	}
	if c.Web.ColorDark == "" {
		c.Web.ColorDark = DefaultStationColorDark
	}
	// Streaming defaults
	if c.Streaming.Outputs == nil {
		c.Streaming.Outputs = []types.Output{}
	}
	for i := range c.Streaming.Outputs {
		if c.Streaming.Outputs[i].CreatedAt == 0 {
			c.Streaming.Outputs[i].CreatedAt = time.Now().UnixMilli()
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

// saveLocked persists configuration. Caller must hold c.mu.
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

// --- Output management ---

// ConfiguredOutputs returns a copy of all outputs.
func (c *Config) ConfiguredOutputs() []types.Output {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Streaming.Outputs)
}

// Output returns a copy of the output with the given ID, or nil if not found.
func (c *Config) Output(id string) *types.Output {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, o := range c.Streaming.Outputs {
		if o.ID == id {
			output := o
			return &output
		}
	}
	return nil
}

// findOutputIndex returns the index of the output with the given ID, or -1 if not found.
func (c *Config) findOutputIndex(id string) int {
	for i, o := range c.Streaming.Outputs {
		if o.ID == id {
			return i
		}
	}
	return -1
}

// AddOutput adds a new output and saves the configuration.
func (c *Config) AddOutput(output *types.Output) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Generate unique ID
	shortID, err := generateShortID()
	if err != nil {
		return fmt.Errorf("failed to generate ID: %w", err)
	}
	output.ID = fmt.Sprintf("output-%s", shortID)

	// New outputs are enabled by default
	output.Enabled = true
	output.CreatedAt = time.Now().UnixMilli()

	c.Streaming.Outputs = append(c.Streaming.Outputs, *output)
	return c.saveLocked()
}

// RemoveOutput removes an output by ID and saves the configuration.
func (c *Config) RemoveOutput(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findOutputIndex(id)
	if i == -1 {
		return fmt.Errorf("output not found: %s", id)
	}

	c.Streaming.Outputs = append(c.Streaming.Outputs[:i], c.Streaming.Outputs[i+1:]...)
	return c.saveLocked()
}

// UpdateOutput updates an existing output and saves the configuration.
func (c *Config) UpdateOutput(output *types.Output) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findOutputIndex(output.ID)
	if i == -1 {
		return fmt.Errorf("output not found: %s", output.ID)
	}

	c.Streaming.Outputs[i] = *output
	return c.saveLocked()
}

// --- Recorder management ---

// Recorder returns a copy of the recorder with the given ID, or nil if not found.
func (c *Config) Recorder(id string) *types.Recorder {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Recording.Recorders {
		if c.Recording.Recorders[i].ID == id {
			recorder := c.Recording.Recorders[i]
			return &recorder
		}
	}
	return nil
}

// findRecorderIndex returns the index of the recorder with the given ID, or -1 if not found.
func (c *Config) findRecorderIndex(id string) int {
	for i := range c.Recording.Recorders {
		if c.Recording.Recorders[i].ID == id {
			return i
		}
	}
	return -1
}

// AddRecorder adds a new recorder and saves the configuration.
func (c *Config) AddRecorder(recorder *types.Recorder) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Generate unique ID
	shortID, err := generateShortID()
	if err != nil {
		return fmt.Errorf("failed to generate ID: %w", err)
	}
	recorder.ID = fmt.Sprintf("recorder-%s", shortID)

	// Apply retention days default if not specified
	if recorder.RetentionDays == 0 {
		recorder.RetentionDays = types.DefaultRetentionDays
	}
	// New recorders are enabled by default
	recorder.Enabled = true
	recorder.CreatedAt = time.Now().UnixMilli()

	c.Recording.Recorders = append(c.Recording.Recorders, *recorder)
	return c.saveLocked()
}

// RemoveRecorder removes a recorder by ID and saves the configuration.
func (c *Config) RemoveRecorder(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findRecorderIndex(id)
	if i == -1 {
		return fmt.Errorf("recorder not found: %s", id)
	}

	c.Recording.Recorders = append(c.Recording.Recorders[:i], c.Recording.Recorders[i+1:]...)
	return c.saveLocked()
}

// UpdateRecorder updates an existing recorder and saves the configuration.
func (c *Config) UpdateRecorder(recorder *types.Recorder) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := c.findRecorderIndex(recorder.ID)
	if i == -1 {
		return fmt.Errorf("recorder not found: %s", recorder.ID)
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

// GetFFmpegPath returns the configured FFmpeg binary path.
func (c *Config) GetFFmpegPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.System.FFmpegPath
}

// LogPath returns the configured log file path for notifications.
func (c *Config) LogPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.Log.Path
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

// GetRecordingAPIKey returns the API key for recording REST endpoints.
func (c *Config) GetRecordingAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Recording.APIKey
}

// --- Setters for individual settings ---

// SetAudioInput updates the audio input device and saves the configuration.
func (c *Config) SetAudioInput(input string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Audio.Input = input
	return c.saveLocked()
}

// SetSilenceThreshold updates the silence detection threshold and saves the configuration.
func (c *Config) SetSilenceThreshold(threshold float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.ThresholdDB = threshold
	return c.saveLocked()
}

// SetSilenceDurationMs updates the silence duration and saves the configuration.
func (c *Config) SetSilenceDurationMs(ms int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.DurationMs = ms
	return c.saveLocked()
}

// SetSilenceRecoveryMs updates the silence recovery time and saves the configuration.
func (c *Config) SetSilenceRecoveryMs(ms int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.RecoveryMs = ms
	return c.saveLocked()
}

// SetWebhookURL updates the webhook URL and saves the configuration.
func (c *Config) SetWebhookURL(url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.Webhook.URL = url
	return c.saveLocked()
}

// SetLogPath updates the log file path and saves the configuration.
func (c *Config) SetLogPath(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.Log.Path = path
	return c.saveLocked()
}

// SetGraphConfig updates all Microsoft Graph/Email configuration fields and saves.
func (c *Config) SetGraphConfig(tenantID, clientID, clientSecret, fromAddress, recipients string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.Email.TenantID = tenantID
	c.Notifications.Email.ClientID = clientID
	c.Notifications.Email.ClientSecret = clientSecret
	c.Notifications.Email.FromAddress = fromAddress
	c.Notifications.Email.Recipients = recipients
	return c.saveLocked()
}

// SetRecordingAPIKey updates the API key and saves the configuration.
func (c *Config) SetRecordingAPIKey(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Recording.APIKey = key
	return c.saveLocked()
}

// --- Snapshot for atomic reads ---

// Snapshot is a point-in-time copy of configuration values.
type Snapshot struct {
	// System
	WebPort     int
	WebUser     string
	WebPassword string

	// Web/Branding
	StationName       string
	StationColorLight string
	StationColorDark  string

	// Audio
	AudioInput string

	// Silence Detection
	SilenceThreshold  float64
	SilenceDurationMs int64
	SilenceRecoveryMs int64

	// Notifications
	WebhookURL        string
	LogPath           string
	GraphTenantID     string
	GraphClientID     string
	GraphClientSecret string
	GraphFromAddress  string
	GraphRecipients   string

	// Recording
	RecordingAPIKey             string
	RecordingMaxDurationMinutes int

	// Entities
	Outputs   []types.Output
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

		// Notifications
		WebhookURL:        c.Notifications.Webhook.URL,
		LogPath:           c.Notifications.Log.Path,
		GraphTenantID:     c.Notifications.Email.TenantID,
		GraphClientID:     c.Notifications.Email.ClientID,
		GraphClientSecret: c.Notifications.Email.ClientSecret,
		GraphFromAddress:  c.Notifications.Email.FromAddress,
		GraphRecipients:   c.Notifications.Email.Recipients,

		// Recording
		RecordingAPIKey:             c.Recording.APIKey,
		RecordingMaxDurationMinutes: cmp.Or(c.Recording.MaxDurationMinutes, DefaultRecordingMaxDurationMinutes),

		// Entities
		Outputs:   slices.Clone(c.Streaming.Outputs),
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

// HasLogPath reports whether a log path is configured.
func (s *Snapshot) HasLogPath() bool {
	return s.LogPath != ""
}

// --- Utility functions ---

// GenerateAPIKey generates a new random 32-character alphanumeric API key.
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

// generateShortID generates a random 8-character hex ID for outputs and recorders.
func generateShortID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
