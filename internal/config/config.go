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

// Configuration defaults.
const (
	DefaultWebPort                     = 8080
	DefaultWebUsername                 = "admin"
	DefaultWebPassword                 = "encoder"
	DefaultSilenceThreshold            = -40.0
	DefaultSilenceDurationMs           = 15000 // 15 seconds in milliseconds
	DefaultSilenceRecoveryMs           = 5000  // 5 seconds in milliseconds
	DefaultEmailSMTPPort               = 587
	DefaultStationName                 = "ZuidWest FM"
	DefaultStationColorLight           = "#E6007E"
	DefaultStationColorDark            = "#E6007E"
	DefaultRecordingMaxDurationMinutes = 240 // 4 hours for on-demand recorders
)

// Default email from name placeholder.
const DefaultEmailFromName = DefaultStationName

// Validation patterns.
var (
	// Station name: any printable characters except control chars (blocks CRLF injection in emails)
	stationNamePattern  = regexp.MustCompile(`^[^\x00-\x1F\x7F]+$`)
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
	ThresholdDB float64 `json:"threshold_db,omitempty"`
	DurationMs  int64   `json:"duration_ms,omitempty"`
	RecoveryMs  int64   `json:"recovery_ms,omitempty"`
}

// NotificationsConfig is the notification configuration.
type NotificationsConfig struct {
	WebhookURL string            `json:"webhook_url,omitempty"`
	LogPath    string            `json:"log_path,omitempty"`
	Email      types.EmailConfig `json:"email,omitempty"`
}

// StationConfig is the station branding configuration.
type StationConfig struct {
	Name       string `json:"name"`
	ColorLight string `json:"color_light"`
	ColorDark  string `json:"color_dark"`
}

// Config holds all application configuration. It is safe for concurrent use.
type Config struct {
	FFmpegPath                  string                 `json:"ffmpeg_path,omitempty"` // Path to FFmpeg binary (empty = use PATH)
	Station                     StationConfig          `json:"station"`
	Web                         WebConfig              `json:"web"`
	Audio                       AudioConfig            `json:"audio"`
	SilenceDetection            SilenceDetectionConfig `json:"silence_detection,omitempty"`
	Notifications               NotificationsConfig    `json:"notifications,omitempty"`
	RecordingAPIKey             string                 `json:"recording_api_key,omitempty"`              // Global API key for all recorders
	RecordingMaxDurationMinutes int                    `json:"recording_max_duration_minutes,omitempty"` // Max duration for on-demand recorders (default 240)
	Outputs                     []types.Output         `json:"outputs"`
	Recorders                   []types.Recorder       `json:"recorders"`

	mu       sync.RWMutex
	filePath string
}

// New creates a new Config with default values.
func New(filePath string) *Config {
	return &Config{
		Station: StationConfig{
			Name:       DefaultStationName,
			ColorLight: DefaultStationColorLight,
			ColorDark:  DefaultStationColorDark,
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
		Recorders:        []types.Recorder{},
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
	name := c.Station.Name
	if name == "" || len(name) > 30 || !stationNamePattern.MatchString(name) {
		return fmt.Errorf("invalid station name %q: must be 1-30 printable characters", name)
	}
	if !stationColorPattern.MatchString(c.Station.ColorLight) {
		return fmt.Errorf("invalid station color_light %q: must be hex format (#RRGGBB)", c.Station.ColorLight)
	}
	if !stationColorPattern.MatchString(c.Station.ColorDark) {
		return fmt.Errorf("invalid station color_dark %q: must be hex format (#RRGGBB)", c.Station.ColorDark)
	}
	return nil
}

// applyDefaults sets default values for zero-value fields.
func (c *Config) applyDefaults() {
	if c.Station.Name == "" {
		c.Station.Name = DefaultStationName
	}
	if c.Station.ColorLight == "" {
		c.Station.ColorLight = DefaultStationColorLight
	}
	if c.Station.ColorDark == "" {
		c.Station.ColorDark = DefaultStationColorDark
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
	if c.Recorders == nil {
		c.Recorders = []types.Recorder{}
	}
	for i := range c.Recorders {
		if c.Recorders[i].Codec == "" {
			c.Recorders[i].Codec = types.DefaultCodec
		}
		if c.Recorders[i].RotationMode == "" {
			c.Recorders[i].RotationMode = types.RotationHourly
		}
		if c.Recorders[i].CreatedAt == 0 {
			c.Recorders[i].CreatedAt = time.Now().UnixMilli()
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

// ConfiguredRecorders returns a copy of all recorders.
func (c *Config) ConfiguredRecorders() []types.Recorder {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Recorders)
}

// Recorder returns a copy of the recorder with the given ID, or nil if not found.
func (c *Config) Recorder(id string) *types.Recorder {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Recorders {
		if c.Recorders[i].ID == id {
			recorder := c.Recorders[i]
			return &recorder
		}
	}
	return nil
}

// findRecorderIndex returns the index of the recorder with the given ID, or -1 if not found.
func (c *Config) findRecorderIndex(id string) int {
	for i := range c.Recorders {
		if c.Recorders[i].ID == id {
			return i
		}
	}
	return -1
}

// AddRecorder adds a new recorder and saves the configuration.
func (c *Config) AddRecorder(recorder *types.Recorder) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if recorder.ID == "" {
		recorder.ID = fmt.Sprintf("recorder-%d", len(c.Recorders)+1)
	}
	if recorder.Codec == "" {
		recorder.Codec = types.DefaultCodec
	}
	if recorder.RotationMode == "" {
		recorder.RotationMode = types.RotationHourly
	}
	if recorder.StorageMode == "" {
		recorder.StorageMode = types.StorageLocal
	}
	if recorder.RetentionDays == 0 {
		recorder.RetentionDays = types.DefaultRetentionDays
	}
	if recorder.Enabled == nil {
		enabled := true
		recorder.Enabled = &enabled
	}
	recorder.CreatedAt = time.Now().UnixMilli()

	c.Recorders = append(c.Recorders, *recorder)
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

	c.Recorders = append(c.Recorders[:i], c.Recorders[i+1:]...)
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

	c.Recorders[i] = *recorder
	return c.saveLocked()
}

// AudioInput returns the configured audio input device.
func (c *Config) AudioInput() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Audio.Input
}

// GetFFmpegPath returns the configured FFmpeg binary path.
// Note: "Get" prefix used to avoid collision with FFmpegPath field.
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
	StationName       string
	StationColorLight string
	StationColorDark  string

	// FFmpeg
	FFmpegPath string

	// Web
	WebPort     int
	WebUser     string
	WebPassword string

	// Audio
	AudioInput string

	// Silence Detection
	SilenceThreshold  float64
	SilenceDurationMs int64
	SilenceRecoveryMs int64

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

	// Recording
	RecordingAPIKey             string
	RecordingMaxDurationMinutes int

	// Outputs (copy)
	Outputs []types.Output

	// Recorders (copy)
	Recorders []types.Recorder
}

// Snapshot returns a point-in-time copy of all configuration values.
func (c *Config) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Snapshot{
		// Station branding
		StationName:       c.Station.Name,
		StationColorLight: c.Station.ColorLight,
		StationColorDark:  c.Station.ColorDark,

		// FFmpeg
		FFmpegPath: c.FFmpegPath,

		// Web
		WebPort:     c.Web.Port,
		WebUser:     c.Web.Username,
		WebPassword: c.Web.Password,

		// Audio
		AudioInput: c.Audio.Input,

		// Silence Detection (with defaults)
		SilenceThreshold:  cmp.Or(c.SilenceDetection.ThresholdDB, DefaultSilenceThreshold),
		SilenceDurationMs: cmp.Or(c.SilenceDetection.DurationMs, DefaultSilenceDurationMs),
		SilenceRecoveryMs: cmp.Or(c.SilenceDetection.RecoveryMs, DefaultSilenceRecoveryMs),

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

		// Recording
		RecordingAPIKey:             c.RecordingAPIKey,
		RecordingMaxDurationMinutes: cmp.Or(c.RecordingMaxDurationMinutes, DefaultRecordingMaxDurationMinutes),

		// Outputs
		Outputs: slices.Clone(c.Outputs),

		// Recorders
		Recorders: slices.Clone(c.Recorders),
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

// GetRecordingAPIKey returns the API key for recording REST endpoints.
// Note: "Get" prefix used to avoid collision with RecordingAPIKey field.
func (c *Config) GetRecordingAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RecordingAPIKey
}

// SetRecordingAPIKey updates the API key and saves the configuration.
func (c *Config) SetRecordingAPIKey(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RecordingAPIKey = key
	return c.saveLocked()
}

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
