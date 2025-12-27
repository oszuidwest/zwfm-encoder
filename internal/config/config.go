// Package config provides application configuration management.
package config

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	DefaultEmailFromName    = "ZuidWest FM Encoder"
)

// WebConfig contains web server configuration.
type WebConfig struct {
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// AudioConfig contains audio input configuration.
type AudioConfig struct {
	Input string `json:"input"`
}

// SilenceDetectionConfig contains silence detection configuration.
type SilenceDetectionConfig struct {
	ThresholdDB     float64 `json:"threshold_db,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	RecoverySeconds float64 `json:"recovery_seconds,omitempty"`
}

// NotificationsConfig contains all notification configuration.
type NotificationsConfig struct {
	WebhookURL string            `json:"webhook_url,omitempty"`
	LogPath    string            `json:"log_path,omitempty"`
	Email      types.EmailConfig `json:"email,omitempty"`
}

// Config holds all application configuration. It is safe for concurrent use.
type Config struct {
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
	return nil
}

// applyDefaults sets default values for zero-value fields.
func (c *Config) applyDefaults() {
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
// Caller must hold c.mu (read or write lock).
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
	if output.Codec == "" {
		output.Codec = types.DefaultCodec
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

// WebPort returns the web server port.
func (c *Config) WebPort() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Web.Port
}

// WebUser returns the web authentication username.
func (c *Config) WebUser() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Web.Username
}

// WebPassword returns the web authentication password.
func (c *Config) WebPassword() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Web.Password
}

// AudioInput returns the configured audio input device.
func (c *Config) AudioInput() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Audio.Input
}

// SetAudioInput updates the audio input device and saves the configuration.
func (c *Config) SetAudioInput(input string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Audio.Input = input
	return c.saveLocked()
}

// SilenceThreshold returns the configured silence threshold in decibels.
func (c *Config) SilenceThreshold() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cmp.Or(c.SilenceDetection.ThresholdDB, DefaultSilenceThreshold)
}

// SetSilenceThreshold updates the silence detection threshold and saves the configuration.
func (c *Config) SetSilenceThreshold(threshold float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.ThresholdDB = threshold
	return c.saveLocked()
}

// SilenceDuration returns the silence duration before alerting.
func (c *Config) SilenceDuration() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cmp.Or(c.SilenceDetection.DurationSeconds, DefaultSilenceDuration)
}

// SetSilenceDuration updates the silence duration and saves the configuration.
func (c *Config) SetSilenceDuration(seconds float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.DurationSeconds = seconds
	return c.saveLocked()
}

// SilenceRecovery returns the audio duration before considering silence recovered.
func (c *Config) SilenceRecovery() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cmp.Or(c.SilenceDetection.RecoverySeconds, DefaultSilenceRecovery)
}

// SetSilenceRecovery updates the silence recovery time and saves the configuration.
func (c *Config) SetSilenceRecovery(seconds float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SilenceDetection.RecoverySeconds = seconds
	return c.saveLocked()
}

// WebhookURL returns the configured webhook URL for notifications.
func (c *Config) WebhookURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.WebhookURL
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

// EmailSMTPHost returns the configured SMTP host.
func (c *Config) EmailSMTPHost() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.Email.Host
}

// EmailSMTPPort returns the configured SMTP port.
func (c *Config) EmailSMTPPort() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cmp.Or(c.Notifications.Email.Port, DefaultEmailSMTPPort)
}

// EmailFromName returns the configured email sender display name.
func (c *Config) EmailFromName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cmp.Or(c.Notifications.Email.FromName, DefaultEmailFromName)
}

// EmailUsername returns the configured SMTP username.
func (c *Config) EmailUsername() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.Email.Username
}

// EmailPassword returns the configured SMTP password.
func (c *Config) EmailPassword() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.Email.Password
}

// EmailRecipients returns the configured email recipients.
func (c *Config) EmailRecipients() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Notifications.Email.Recipients
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

// Snapshot contains a point-in-time copy of all configuration values.
// Use this instead of multiple individual getters to reduce mutex contention.
type Snapshot struct {
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
		EmailFromName:   cmp.Or(c.Notifications.Email.FromName, DefaultEmailFromName),
		EmailUsername:   c.Notifications.Email.Username,
		EmailPassword:   c.Notifications.Email.Password,
		EmailRecipients: c.Notifications.Email.Recipients,

		// Outputs
		Outputs: slices.Clone(c.Outputs),
	}
}

// HasWebhook returns true if a webhook URL is configured.
func (s *Snapshot) HasWebhook() bool {
	return s.WebhookURL != ""
}

// HasEmail returns true if email notifications are configured.
func (s *Snapshot) HasEmail() bool {
	return s.EmailSMTPHost != "" && s.EmailRecipients != ""
}

// HasLogPath returns true if a log path is configured.
func (s *Snapshot) HasLogPath() bool {
	return s.LogPath != ""
}
