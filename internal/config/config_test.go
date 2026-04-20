package config

import (
	"os"
	"path/filepath"
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
    "threshold_db": 0,
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
	if snap.SilenceThreshold != 0 {
		t.Fatalf("SilenceThreshold = %v, want 0", snap.SilenceThreshold)
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
