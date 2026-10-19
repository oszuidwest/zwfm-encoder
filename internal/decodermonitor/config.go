// Package decodermonitor provides a local, observe-only MicroMPX watchdog.
// It never opens an audio PCM device or controls the decoder service.
package decodermonitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const maxDocument = 2 << 20

var pcmPath = regexp.MustCompile(`^/proc/asound/[a-zA-Z0-9_-]+/pcm\d+p/sub\d+/status$`)

// Config is independent of the encoder configuration and belongs on each decoder.
// Sending mail is opt-in; neither check mode nor the default preview mode sends it.
type Config struct {
	Site               string `json:"site"`
	PCMStatus          string `json:"pcm_status"`
	StateDir           string `json:"state_dir"`
	EmailFile          string `json:"email_file"`
	SendMail           bool   `json:"send_mail"`
	HeartbeatURL       string `json:"heartbeat_url,omitempty"`
	PollSeconds        int    `json:"poll_seconds"`
	RecoverySeconds    int    `json:"recovery_seconds"`
	ReminderSeconds    int    `json:"reminder_seconds"`
	UnknownSeconds     int    `json:"unknown_seconds"`
	CriticalCount      int    `json:"critical_count"`
	CriticalWindowSecs int    `json:"critical_window_seconds"`
	StalledChecks      int    `json:"stalled_checks"`
	RetentionDays      int    `json:"retention_days"`
	EvidenceMiB        int    `json:"evidence_mib"`
}

// DefaultConfig supplies provisional, configurable incident thresholds.
func DefaultConfig() Config {
	return Config{
		PCMStatus: "/proc/asound/sndrpihifiberry/pcm0p/sub0/status",
		StateDir:  "/var/lib/micrompx-watchdog", PollSeconds: 10,
		RecoverySeconds: 900, ReminderSeconds: 900, UnknownSeconds: 60,
		CriticalCount: 3, CriticalWindowSecs: 60, StalledChecks: 2,
		RetentionDays: 7, EvidenceMiB: 64,
	}
}

// LoadConfig reads an existing private configuration, without creating defaults.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if err := readPrivateJSON(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("read watchdog configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects settings that could disable bounds or confuse audio identity.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("watchdog configuration is required")
	}
	if strings.TrimSpace(c.Site) == "" || len(c.Site) > 100 || strings.ContainsAny(c.Site, "\r\n\x00") {
		return fmt.Errorf("site must be a non-empty single line of at most 100 bytes")
	}
	if !pcmPath.MatchString(c.PCMStatus) {
		return fmt.Errorf("pcm_status must identify one playback status file under /proc/asound")
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) == string(filepath.Separator) {
		return fmt.Errorf("state_dir must be a dedicated absolute directory")
	}
	if c.SendMail && !filepath.IsAbs(c.EmailFile) {
		return fmt.Errorf("email_file must be an absolute path when send_mail is enabled")
	}
	for name, limit := range map[string][3]int{
		"poll_seconds": {c.PollSeconds, 10, 60}, "recovery_seconds": {c.RecoverySeconds, 60, 86400},
		"reminder_seconds": {c.ReminderSeconds, 60, 86400}, "unknown_seconds": {c.UnknownSeconds, c.PollSeconds, 3600},
		"critical_count": {c.CriticalCount, 2, 100}, "critical_window_seconds": {c.CriticalWindowSecs, 10, 3600},
		"stalled_checks": {c.StalledChecks, 2, 10}, "retention_days": {c.RetentionDays, 1, 90},
		"evidence_mib": {c.EvidenceMiB, 4, 1024},
	} {
		if limit[0] < limit[1] || limit[0] > limit[2] {
			return fmt.Errorf("%s must be between %d and %d", name, limit[1], limit[2])
		}
	}
	if c.HeartbeatURL != "" {
		u, err := url.Parse(c.HeartbeatURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("heartbeat_url must be an https URL without userinfo or fragment")
		}
	}
	return nil
}

// LoadEmail reads only the existing Graph configuration shape, not encoder settings.
func LoadEmail(path string) (types.GraphConfig, error) {
	var cfg types.GraphConfig
	if err := readPrivateJSON(path, &cfg); err != nil {
		return cfg, fmt.Errorf("read M365 configuration: %w", err)
	}
	if err := notify.ValidateConfig(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid M365 configuration: %w", err)
	}
	return cfg, nil
}

func readPrivateJSON(path string, target any) error {
	f, err := os.Open(path) //nolint:gosec // Administrator-selected configuration path, never remote input.
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("configuration must be a regular file readable only by its owner (mode 0600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return fmt.Errorf("configuration cannot be read or exceeds 64 KiB")
	}
	if err := validateJSON(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		// Decoder errors can quote a secret supplied with the wrong type.
		return fmt.Errorf("configuration has unknown fields or invalid value types")
	}
	return nil
}

func validateJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(dec); err != nil {
		return fmt.Errorf("configuration must be an object without duplicate keys or null values")
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("configuration must contain exactly one JSON object")
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return fmt.Errorf("configuration must be an object")
	}
	return nil
}

func walkJSON(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil || token == nil {
		return fmt.Errorf("invalid JSON")
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate key")
			}
			seen[name] = true
			if err := walkJSON(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	case json.Delim('['):
		for dec.More() {
			if err := walkJSON(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	}
	return err
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }
