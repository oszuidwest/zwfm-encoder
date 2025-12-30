// Package config provides application configuration management.
package config

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Configuration defaults.
const (
	DefaultWebPort          = 8080
	DefaultWebUsername      = "admin"
	DefaultWebPassword      = "encoder"
	DefaultSilenceThreshold = -40.0
	DefaultSilenceDuration  = 15.0
	DefaultSilenceRecovery  = 5.0
	DefaultEmailSMTPPort    = 587
	DefaultStationName      = "ZuidWest FM"
	DefaultStationColor     = "#E6007E"
)

// Default email from name placeholder.
const DefaultEmailFromName = DefaultStationName

// Validation patterns.
var (
	stationNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9 ]{1,30}$`)
	stationColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// WebConfig is the web server configuration.
type WebConfig struct {
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// AudioConfig is the audio input configuration.
type AudioConfig struct {
	Input string `json:"input"`
}

// SilenceDetectionConfig is the silence detection configuration.
type SilenceDetectionConfig struct {
	ThresholdDB     float64 `json:"threshold_db,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	RecoverySeconds float64 `json:"recovery_seconds,omitempty"`
}

// NotificationsConfig is the notification configuration.
type NotificationsConfig struct {
	WebhookURL string            `json:"webhook_url,omitempty"`
	LogPath    string            `json:"log_path,omitempty"`
	Email      types.EmailConfig `json:"email,omitempty"`
}

// StationConfig is the station branding configuration.
type StationConfig struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Config holds all application configuration. It is safe for concurrent use.
type Config struct {
	FFmpegPath       string                 `json:"ffmpeg_path,omitempty"` // Path to FFmpeg binary (empty = use PATH)
	Station          StationConfig          `json:"station"`
	Web              WebConfig              `json:"web"`
	Audio            AudioConfig            `json:"audio"`
	SilenceDetection SilenceDetectionConfig `json:"silence_detection,omitempty"`
	Notifications    NotificationsConfig    `json:"notifications,omitempty"`
	Outputs          []types.Output         `json:"outputs"`

	mu       sync.RWMutex
	filePath string
}

// New creates a new Config with default values.
func New(filePath string) *Config {
	return &Config{
		Station: StationConfig{
			Name:  DefaultStationName,
			Color: DefaultStationColor,
		},
		Web: WebConfig{
			Port:     DefaultWebPort,
			Username: DefaultWebUsername,
			Password: DefaultWebPassword,
		},
		Audio:            AudioConfig{},
		SilenceDetection: SilenceDetectionConfig{},
		Notifications:    NotificationsConfig{},
		Outputs:          []types.Output{},
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

	if err := c.validateStation(); err != nil {
		return err
	}

	return nil
}

// validateStation validates station configuration.
func (c *Config) validateStation() error {
	if !stationNamePattern.MatchString(c.Station.Name) {
		return fmt.Errorf("invalid station name %q: must be 1-30 alphanumeric characters or spaces", c.Station.Name)
	}
	if !stationColorPattern.MatchString(c.Station.Color) {
		return fmt.Errorf("invalid station color %q: must be hex format (#RRGGBB)", c.Station.Color)
	}
	return nil
}

// applyDefaults sets default values for zero-value fields.
func (c *Config) applyDefaults() {
	if c.Station.Name == "" {
		c.Station.Name = DefaultStationName
	}
	if c.Station.Color == "" {
		c.Station.Color = DefaultStationColor
	}
	if c.Web.Port == 0 {
		c.Web.Port = DefaultWebPort
	}
	if c.Web.Username == "" {
		c.Web.Username = DefaultWebUsername
	}
	if c.Web.Password == "" {
		c.Web.Password = DefaultWebPassword
	}
	if c.Outputs == nil {
		c.Outputs = []types.Output{}
	}
	for i := range c.Outputs {
		if c.Outputs[i].Codec == "" {
			c.Outputs[i].Codec = types.DefaultCodec
		}
		if c.Outputs[i].CreatedAt == 0 {
			c.Outputs[i].CreatedAt = time.Now().UnixMilli()
		}
	}
}

// Save writes the configuration to file.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
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

// ConfiguredOutputs returns a copy of all outputs.
func (c *Config) ConfiguredOutputs() []types.Output {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Outputs)
}

// Output returns a copy of the output with the given ID, or nil if not found.
func (c *Config) Output(id string) *types.Output {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, o := range c.Outputs {
		if o.ID == id {
			output := o
			return &output
		}
	}
	return nil
}

// findOutputIndex returns the index of the output with the given ID, or -1 if not found.
func (c *Config) findOutputIndex(id string) int {
	for i, o := range c.Outputs {
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

	if output.ID == "" {
		output.ID = fmt.Sprintf("output-%d", len(c.Outputs)+1)
	}
	// Codec default is handled by validateOutput in the command handler
	if output.Enabled == nil {
		enabled := true
		output.Enabled = &enabled
	}
	output.CreatedAt = time.Now().UnixMilli()

	c.Outputs = append(c.Outputs, *output)
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

	c.Outputs = append(c.Outputs[:i], c.Outputs[i+1:]...)
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

	c.Outputs[i] = *output
	return c.saveLocked()
}

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
	return c.FFmpegPath
}

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

// SetSilenceDuration updates the silence duration and saves the configuration.
func (c *Config) SetSilenceDuration(seconds float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.DurationSeconds = seconds
	return c.saveLocked()
}

// SetSilenceRecovery updates the silence recovery time and saves the configuration.
func (c *Config) SetSilenceRecovery(seconds float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.RecoverySeconds = seconds
	return c.saveLocked()
}

// SetWebhookURL updates the webhook URL and saves the configuration.
func (c *Config) SetWebhookURL(url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.WebhookURL = url
	return c.saveLocked()
}

// LogPath returns the configured log file path for notifications.
func (c *Config) LogPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.LogPath
}

// SetLogPath updates the log file path and saves the configuration.
func (c *Config) SetLogPath(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.LogPath = path
	return c.saveLocked()
}

// SetEmailConfig updates all email configuration fields and saves.
func (c *Config) SetEmailConfig(host string, port int, fromName, username, password, recipients string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Notifications.Email.Host = host
	c.Notifications.Email.Port = port
	c.Notifications.Email.FromName = fromName
	c.Notifications.Email.Username = username
	c.Notifications.Email.Password = password
	c.Notifications.Email.Recipients = recipients
	return c.saveLocked()
}

// Snapshot is a point-in-time copy of configuration values.
type Snapshot struct {
	// Station branding
	StationName  string
	StationColor string

	// FFmpeg
	FFmpegPath string

	// Web
	WebPort     int
	WebUser     string
	WebPassword string

	// Audio
	AudioInput string

	// Silence Detection
	SilenceThreshold float64
	SilenceDuration  float64
	SilenceRecovery  float64

	// Notifications
	WebhookURL string
	LogPath    string

	// Email
	EmailSMTPHost   string
	EmailSMTPPort   int
	EmailFromName   string
	EmailUsername   string
	EmailPassword   string
	EmailRecipients string

	// Outputs (copy)
	Outputs []types.Output
}

// Snapshot returns a point-in-time copy of all configuration values.
func (c *Config) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Snapshot{
		// Station branding
		StationName:  c.Station.Name,
		StationColor: c.Station.Color,

		// FFmpeg
		FFmpegPath: c.FFmpegPath,

		// Web
		WebPort:     c.Web.Port,
		WebUser:     c.Web.Username,
		WebPassword: c.Web.Password,

		// Audio
		AudioInput: c.Audio.Input,

		// Silence Detection (with defaults)
		SilenceThreshold: cmp.Or(c.SilenceDetection.ThresholdDB, DefaultSilenceThreshold),
		SilenceDuration:  cmp.Or(c.SilenceDetection.DurationSeconds, DefaultSilenceDuration),
		SilenceRecovery:  cmp.Or(c.SilenceDetection.RecoverySeconds, DefaultSilenceRecovery),

		// Notifications
		WebhookURL: c.Notifications.WebhookURL,
		LogPath:    c.Notifications.LogPath,

		// Email (with defaults)
		EmailSMTPHost:   c.Notifications.Email.Host,
		EmailSMTPPort:   cmp.Or(c.Notifications.Email.Port, DefaultEmailSMTPPort),
		EmailFromName:   cmp.Or(c.Notifications.Email.FromName, c.Station.Name),
		EmailUsername:   c.Notifications.Email.Username,
		EmailPassword:   c.Notifications.Email.Password,
		EmailRecipients: c.Notifications.Email.Recipients,

		// Outputs
		Outputs: slices.Clone(c.Outputs),
	}
}

// HasWebhook reports whether a webhook URL is configured.
func (s *Snapshot) HasWebhook() bool {
	return s.WebhookURL != ""
}

// HasEmail reports whether email notifications are configured.
func (s *Snapshot) HasEmail() bool {
	return s.EmailSMTPHost != "" && s.EmailRecipients != ""
}

// HasLogPath reports whether a log path is configured.
func (s *Snapshot) HasLogPath() bool {
	return s.LogPath != ""
}
