package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestLoadCreatesDefaultConfig(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := New(configPath)

	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}

	snap := cfg.Snapshot()
	if snap.WebPort != DefaultWebPort {
		t.Fatalf("WebPort = %d, want %d", snap.WebPort, DefaultWebPort)
	}
	if snap.SilenceThreshold != DefaultSilenceThreshold {
		t.Fatalf("SilenceThreshold = %v, want %v", snap.SilenceThreshold, DefaultSilenceThreshold)
	}
	if snap.SilenceDurationMs != DefaultSilenceDurationMs {
		t.Fatalf("SilenceDurationMs = %d, want %d", snap.SilenceDurationMs, DefaultSilenceDurationMs)
	}
	if snap.SilenceRecoveryMs != DefaultSilenceRecoveryMs {
		t.Fatalf("SilenceRecoveryMs = %d, want %d", snap.SilenceRecoveryMs, DefaultSilenceRecoveryMs)
	}
	if snap.PeakHoldMs != DefaultPeakHoldMs {
		t.Fatalf("PeakHoldMs = %d, want %d", snap.PeakHoldMs, DefaultPeakHoldMs)
	}
	if !snap.SilenceDumpEnabled {
		t.Fatal("SilenceDumpEnabled = false, want true")
	}
	if snap.SilenceDumpRetentionDays != types.DefaultSilenceDumpRetentionDays {
		t.Fatalf("SilenceDumpRetentionDays = %d, want %d", snap.SilenceDumpRetentionDays, types.DefaultSilenceDumpRetentionDays)
	}
	if snap.ZabbixPort != DefaultZabbixPort {
		t.Fatalf("ZabbixPort = %d, want %d", snap.ZabbixPort, DefaultZabbixPort)
	}
	if snap.RecordingMaxDurationMinutes != DefaultRecordingMaxDurationMinutes {
		t.Fatalf("RecordingMaxDurationMinutes = %d, want %d", snap.RecordingMaxDurationMinutes, DefaultRecordingMaxDurationMinutes)
	}
	if !snap.WebhookEvents.SilenceStart || !snap.WebhookEvents.SilenceEnd || !snap.WebhookEvents.AudioDump {
		t.Fatalf("WebhookEvents = %+v, want all true", snap.WebhookEvents)
	}
	if !snap.EmailEvents.SilenceStart || !snap.EmailEvents.SilenceEnd || !snap.EmailEvents.AudioDump {
		t.Fatalf("EmailEvents = %+v, want all true", snap.EmailEvents)
	}
	if !snap.ZabbixEvents.SilenceStart || !snap.ZabbixEvents.SilenceEnd {
		t.Fatalf("ZabbixEvents = %+v, want both true", snap.ZabbixEvents)
	}
}

func TestLoadPreservesExplicitZeroAndFalseValues(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "silence_detection": {
    "threshold_db": -1,
    "duration_ms": 1234,
    "recovery_ms": 4321,
    "peak_hold_ms": 999
  },
  "silence_dump": {
    "enabled": false,
    "retention_days": 0
  },
  "notifications": {
    "webhook": {
      "events": {
        "silence_start": false,
        "silence_end": false,
        "audio_dump": false
      }
    },
    "email": {
      "events": {
        "silence_start": false,
        "silence_end": false,
        "audio_dump": false
      }
    },
    "zabbix": {
      "port": 0,
      "events": null
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	snap := cfg.Snapshot()
	if snap.SilenceThreshold != -1 {
		t.Fatalf("SilenceThreshold = %v, want -1", snap.SilenceThreshold)
	}
	if snap.SilenceDurationMs != 1234 {
		t.Fatalf("SilenceDurationMs = %d, want 1234", snap.SilenceDurationMs)
	}
	if snap.SilenceRecoveryMs != 4321 {
		t.Fatalf("SilenceRecoveryMs = %d, want 4321", snap.SilenceRecoveryMs)
	}
	if snap.PeakHoldMs != 999 {
		t.Fatalf("PeakHoldMs = %d, want 999", snap.PeakHoldMs)
	}
	if snap.SilenceDumpEnabled {
		t.Fatal("SilenceDumpEnabled = true, want false")
	}
	if snap.SilenceDumpRetentionDays != 0 {
		t.Fatalf("SilenceDumpRetentionDays = %d, want 0", snap.SilenceDumpRetentionDays)
	}
	if snap.WebhookEvents != (types.EventSubscriptions{}) {
		t.Fatalf("WebhookEvents = %+v, want all false", snap.WebhookEvents)
	}
	if snap.EmailEvents != (types.EventSubscriptions{}) {
		t.Fatalf("EmailEvents = %+v, want all false", snap.EmailEvents)
	}
	if snap.ZabbixEvents != (types.ZabbixEventSubscriptions{}) {
		t.Fatalf("ZabbixEvents = %+v, want zero value after nil guard", snap.ZabbixEvents)
	}
	if snap.ZabbixPort != 0 {
		t.Fatalf("ZabbixPort = %d, want 0", snap.ZabbixPort)
	}
	if snap.WebPort != DefaultWebPort {
		t.Fatalf("WebPort = %d, want default %d for missing field", snap.WebPort, DefaultWebPort)
	}
	if snap.RecordingMaxDurationMinutes != DefaultRecordingMaxDurationMinutes {
		t.Fatalf("RecordingMaxDurationMinutes = %d, want default %d for missing field", snap.RecordingMaxDurationMinutes, DefaultRecordingMaxDurationMinutes)
	}
}

func TestLoadRejectsInvalidFileSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "positive silence threshold",
			data:    `{"silence_detection":{"threshold_db":1}}`,
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "zero silence threshold",
			data:    `{"silence_detection":{"threshold_db":0}}`,
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "fractional silence threshold above -1",
			data:    `{"silence_detection":{"threshold_db":-0.5}}`,
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "silence threshold below -60",
			data:    `{"silence_detection":{"threshold_db":-61}}`,
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "zero silence duration",
			data:    `{"silence_detection":{"duration_ms":0}}`,
			wantErr: "silence_detection.duration_ms: must be greater than 0",
		},
		{
			name:    "zero silence recovery",
			data:    `{"silence_detection":{"recovery_ms":0}}`,
			wantErr: "silence_detection.recovery_ms: must be greater than 0",
		},
		{
			name:    "zero peak hold",
			data:    `{"silence_detection":{"peak_hold_ms":0}}`,
			wantErr: "silence_detection.peak_hold_ms: must be greater than 0",
		},
		{
			name:    "negative silence dump retention",
			data:    `{"silence_dump":{"retention_days":-1}}`,
			wantErr: "silence_dump.retention_days: cannot be negative",
		},
		{
			name:    "invalid webhook url",
			data:    `{"notifications":{"webhook":{"url":"://broken"}}}`,
			wantErr: "notifications.webhook.url: invalid URL format",
		},
		{
			name:    "invalid graph from address",
			data:    `{"notifications":{"email":{"from_address":"not-an-email"}}}`,
			wantErr: "notifications.email.from_address: invalid email format",
		},
		{
			name:    "invalid graph recipient",
			data:    `{"notifications":{"email":{"recipients":"good@example.com, bad-address"}}}`,
			wantErr: "notifications.email.recipients: contains invalid email address",
		},
		{
			name:    "invalid zabbix port",
			data:    `{"notifications":{"zabbix":{"port":70000}}}`,
			wantErr: "notifications.zabbix.port: must be between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(tt.data), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg := New(configPath)
			err := cfg.Load()
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestSettingsUpdateValidateAPIFieldNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		update  SettingsUpdate
		wantErr string
	}{
		{
			name:    "positive silence threshold uses API name",
			update:  SettingsUpdate{SilenceThreshold: 1, SilenceDurationMs: 1, SilenceRecoveryMs: 1},
			wantErr: "silence_threshold:",
		},
		{
			name:    "zero silence threshold uses API name",
			update:  SettingsUpdate{SilenceThreshold: 0, SilenceDurationMs: 1, SilenceRecoveryMs: 1},
			wantErr: "silence_threshold:",
		},
		{
			name:    "fractional silence threshold above -1 uses API name",
			update:  SettingsUpdate{SilenceThreshold: -0.5, SilenceDurationMs: 1, SilenceRecoveryMs: 1},
			wantErr: "silence_threshold:",
		},
		{
			name:    "zero silence duration uses API name",
			update:  SettingsUpdate{SilenceThreshold: -40, SilenceDurationMs: 0, SilenceRecoveryMs: 1},
			wantErr: "silence_duration_ms:",
		},
		{
			name:    "invalid webhook url uses API name",
			update:  SettingsUpdate{SilenceThreshold: -40, SilenceDurationMs: 1, SilenceRecoveryMs: 1, WebhookURL: "://broken"},
			wantErr: "webhook_url:",
		},
		{
			name:    "invalid graph from address uses API name",
			update:  SettingsUpdate{SilenceThreshold: -40, SilenceDurationMs: 1, SilenceRecoveryMs: 1, GraphFromAddress: "not-an-email"},
			wantErr: "graph_from_address:",
		},
		{
			name:    "invalid graph recipients uses API name",
			update:  SettingsUpdate{SilenceThreshold: -40, SilenceDurationMs: 1, SilenceRecoveryMs: 1, GraphRecipients: "bad-address"},
			wantErr: "graph_recipients:",
		},
		{
			name:    "invalid zabbix port uses API name",
			update:  SettingsUpdate{SilenceThreshold: -40, SilenceDurationMs: 1, SilenceRecoveryMs: 1, ZabbixPort: 70000},
			wantErr: "zabbix_port:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errs := tt.update.Validate()
			if len(errs) == 0 {
				t.Fatal("Validate() returned no errors, want at least one")
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e, tt.wantErr) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Validate() errors = %v, want one containing %q", errs, tt.wantErr)
			}
		})
	}
}

func TestSaveLockedWritePath(t *testing.T) {
	t.Parallel()

	t.Run("permissions are 0600 after save", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		cfg := New(configPath)
		if err := cfg.Load(); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file permissions = %o, want 0600", got)
		}
	})

	t.Run("no temp files left after successful save", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		cfg := New(configPath)
		if err := cfg.Load(); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir() error = %v", err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("temp file not cleaned up: %s", e.Name())
			}
		}
	})

	t.Run("config is valid JSON after save", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		cfg := New(configPath)
		if err := cfg.Load(); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		data, err := os.ReadFile(configPath) //nolint:gosec // G304: test reads a controlled temp path
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatalf("saved config is not valid JSON: %v", err)
		}
	})
}

func TestSilenceThresholdBoundary(t *testing.T) {
	t.Parallel()

	t.Run("minus one is valid in file config", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(configPath, []byte(`{"silence_detection":{"threshold_db":-1}}`), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		cfg := New(configPath)
		if err := cfg.Load(); err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
	})

	t.Run("minus sixty is valid in file config", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(configPath, []byte(`{"silence_detection":{"threshold_db":-60}}`), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		cfg := New(configPath)
		if err := cfg.Load(); err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
	})

	t.Run("minus one is valid in API update", func(t *testing.T) {
		t.Parallel()

		u := SettingsUpdate{SilenceThreshold: -1, SilenceDurationMs: 1, SilenceRecoveryMs: 1}
		errs := u.Validate()
		for _, e := range errs {
			if strings.Contains(e, "silence_threshold") {
				t.Fatalf("Validate() returned unexpected silence_threshold error: %q", e)
			}
		}
	})

	t.Run("minus sixty is valid in API update", func(t *testing.T) {
		t.Parallel()

		u := SettingsUpdate{SilenceThreshold: -60, SilenceDurationMs: 1, SilenceRecoveryMs: 1}
		errs := u.Validate()
		for _, e := range errs {
			if strings.Contains(e, "silence_threshold") {
				t.Fatalf("Validate() returned unexpected silence_threshold error: %q", e)
			}
		}
	})
}
