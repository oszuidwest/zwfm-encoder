package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	assertDefaultScalarSettings(t, &snap)
	assertDefaultEventSettings(t, &snap)
}

func assertDefaultScalarSettings(t *testing.T, snap *Snapshot) {
	t.Helper()

	assertEqual(t, "WebPort", snap.WebPort, DefaultWebPort)
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, DefaultSilenceThreshold)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, DefaultSilenceDurationMs)
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, DefaultSilenceRecoveryMs)
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, DefaultPeakHoldMs)
	assertTrue(t, "SilenceDumpEnabled", snap.SilenceDumpEnabled)
	assertEqual(t, "SilenceDumpRetentionDays", snap.SilenceDumpRetentionDays, types.DefaultSilenceDumpRetentionDays)
	assertEqual(t, "ZabbixPort", snap.ZabbixPort, DefaultZabbixPort)
	assertEqual(t, "RecordingMaxDurationMinutes", snap.RecordingMaxDurationMinutes, DefaultRecordingMaxDurationMinutes)
}

func assertDefaultEventSettings(t *testing.T, snap *Snapshot) {
	t.Helper()

	defaultEvents := types.EventSubscriptions{SilenceStart: true, SilenceEnd: true, AudioDump: true}
	assertEqual(t, "WebhookEvents", snap.WebhookEvents, defaultEvents)
	assertEqual(t, "EmailEvents", snap.EmailEvents, defaultEvents)
	assertEqual(t, "WhatsAppEvents", snap.WhatsAppEvents, defaultEvents)
	assertEqual(t, "ZabbixEvents", snap.ZabbixEvents, types.EventSubscriptions{SilenceStart: true, SilenceEnd: true})
}

func assertEqual[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()

	if got != want {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func assertTrue(t *testing.T, name string, got bool) {
	t.Helper()

	if !got {
		t.Fatalf("%s = false, want true", name)
	}
}

func TestSnapshotHasWhatsApp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap Snapshot
		want bool
	}{
		{
			name: "fully configured",
			snap: Snapshot{
				WhatsAppPhoneNumberID: "12345",
				WhatsAppAccessToken:   "token",
				WhatsAppRecipients:    "+31612345678",
			},
			want: true,
		},
		{
			name: "empty split recipients",
			snap: Snapshot{
				WhatsAppPhoneNumberID: "12345",
				WhatsAppAccessToken:   "token",
				WhatsAppRecipients:    ",,, ",
			},
			want: false,
		},
		{
			name: "whitespace token",
			snap: Snapshot{
				WhatsAppPhoneNumberID: "12345",
				WhatsAppAccessToken:   " ",
				WhatsAppRecipients:    "+31612345678",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.snap.HasWhatsApp(); got != tt.want {
				t.Fatalf("HasWhatsApp() = %v, want %v", got, tt.want)
			}
		})
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
    "whatsapp": {
      "events": {
        "silence_start": false,
        "silence_end": false,
        "audio_dump": false
      }
    },
    "zabbix": {
      "port": 0,
      "events": {"silence_start": false, "silence_end": false}
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
	if snap.WhatsAppEvents != (types.EventSubscriptions{}) {
		t.Fatalf("WhatsAppEvents = %+v, want all false", snap.WhatsAppEvents)
	}
	if snap.ZabbixEvents != (types.EventSubscriptions{}) {
		t.Fatalf("ZabbixEvents = %+v, want both false", snap.ZabbixEvents)
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

func TestZabbixEventsJSONSemantics(t *testing.T) {
	t.Parallel()

	defaults := types.EventSubscriptions{SilenceStart: true, SilenceEnd: true}
	allFalse := types.EventSubscriptions{}

	tests := []struct {
		name string
		json string
		want types.EventSubscriptions
	}{
		{
			name: "missing events field preserves defaults",
			json: `{"notifications":{"zabbix":{}}}`,
			want: defaults,
		},
		{
			name: "events null is a no-op, preserves defaults",
			json: `{"notifications":{"zabbix":{"events":null}}}`,
			want: defaults,
		},
		{
			name: "events empty object is a no-op, preserves defaults",
			json: `{"notifications":{"zabbix":{"events":{}}}}`,
			want: defaults,
		},
		{
			name: "partial object only overrides present fields",
			json: `{"notifications":{"zabbix":{"events":{"silence_start":false}}}}`,
			want: types.EventSubscriptions{SilenceStart: false, SilenceEnd: true},
		},
		{
			name: "explicit false values disable events",
			json: `{"notifications":{"zabbix":{"events":{"silence_start":false,"silence_end":false}}}}`,
			want: allFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(tt.json), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg := New(configPath)
			if err := cfg.Load(); err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			got := cfg.Snapshot().ZabbixEvents
			if got != tt.want {
				t.Fatalf("ZabbixEvents = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestZabbixEventsRoundTrip(t *testing.T) {
	t.Parallel()

	// Load a config with events: null (no-op, defaults should apply).
	// Then trigger an unrelated save via AddStream (which calls saveLocked directly,
	// bypassing ApplySettings) and verify defaults survive reload.
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"notifications":{"zabbix":{"events":null}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Trigger saveLocked via AddStream without touching settings.
	stream := &types.Stream{Host: "127.0.0.1", Port: 1234}
	if err := cfg.AddStream(stream); err != nil {
		t.Fatalf("AddStream() error = %v", err)
	}

	// Reload into a fresh config to simulate a server restart.
	cfg2 := New(configPath)
	if err := cfg2.Load(); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}

	got := cfg2.Snapshot().ZabbixEvents
	want := types.EventSubscriptions{SilenceStart: true, SilenceEnd: true}
	if got != want {
		t.Fatalf("ZabbixEvents after round-trip = %+v, want %+v", got, want)
	}
}

func TestApplySettingsValidatesInput(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := New(configPath)

	before := cfg.Snapshot()
	err := cfg.ApplySettings(&SettingsUpdate{
		AudioInput:        "test-device",
		SilenceThreshold:  0,
		SilenceDurationMs: 15000,
		SilenceRecoveryMs: 5000,
	})
	if err == nil {
		t.Fatal("ApplySettings() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "invalid settings:") {
		t.Fatalf("ApplySettings() error = %v, want aggregated validation error", err)
	}

	after := cfg.Snapshot()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Snapshot changed after rejected ApplySettings:\n got  %+v\n want %+v", after, before)
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
			wantErr: "silence_detection.peak_hold_ms: must be between 500 and 10000 ms",
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
			name:    "invalid whatsapp recipient",
			data:    `{"notifications":{"whatsapp":{"recipients":"+31612345678, bad-address"}}}`,
			wantErr: "notifications.whatsapp.recipients: contains invalid phone number",
		},
		{
			name:    "whatsapp phone number id required",
			data:    `{"notifications":{"whatsapp":{"access_token":"token","recipients":"+31612345678"}}}`,
			wantErr: "notifications.whatsapp.phone_number_id: is required when WhatsApp is configured",
		},
		{
			name:    "whatsapp phone number id digits only",
			data:    `{"notifications":{"whatsapp":{"phone_number_id":"abc","access_token":"token","recipients":"+31612345678"}}}`,
			wantErr: "notifications.whatsapp.phone_number_id: must contain digits only",
		},
		{
			name:    "whatsapp access token required",
			data:    `{"notifications":{"whatsapp":{"phone_number_id":"12345","recipients":"+31612345678"}}}`,
			wantErr: "notifications.whatsapp.access_token: is required when WhatsApp is configured",
		},
		{
			name:    "whatsapp split recipients required",
			data:    `{"notifications":{"whatsapp":{"phone_number_id":"12345","access_token":"token","recipients":",,, "}}}`,
			wantErr: "notifications.whatsapp.recipients: is required when WhatsApp is configured",
		},
		{
			name:    "whatsapp template language requires template name",
			data:    `{"notifications":{"whatsapp":{"phone_number_id":"12345","access_token":"token","recipients":"+31612345678","template_language":"nl"}}}`,
			wantErr: "notifications.whatsapp.template_language: requires notifications.whatsapp.template_name",
		},
		{
			name:    "whatsapp template name format",
			data:    `{"notifications":{"whatsapp":{"phone_number_id":"12345","access_token":"token","recipients":"+31612345678","template_name":"Encoder Alert"}}}`,
			wantErr: "notifications.whatsapp.template_name: must contain only lowercase letters, digits, and underscores",
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
			name: "invalid whatsapp recipients uses API name",
			update: SettingsUpdate{
				SilenceThreshold:   -40,
				SilenceDurationMs:  1,
				SilenceRecoveryMs:  1,
				WhatsAppRecipients: "bad-address",
			},
			wantErr: "whatsapp_recipients:",
		},
		{
			name: "missing whatsapp token uses API name",
			update: SettingsUpdate{
				SilenceThreshold:      -40,
				SilenceDurationMs:     1,
				SilenceRecoveryMs:     1,
				WhatsAppPhoneNumberID: "12345",
				WhatsAppRecipients:    "+31612345678",
			},
			wantErr: "whatsapp_access_token:",
		},
		{
			name: "invalid whatsapp template name uses API name",
			update: SettingsUpdate{
				SilenceThreshold:      -40,
				SilenceDurationMs:     1,
				SilenceRecoveryMs:     1,
				WhatsAppPhoneNumberID: "12345",
				WhatsAppAccessToken:   "token",
				WhatsAppRecipients:    "+31612345678",
				WhatsAppTemplateName:  "Encoder Alert",
			},
			wantErr: "whatsapp_template_name:",
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

// minimalValidUpdate returns a SettingsUpdate with just the four range-required
// fields filled (silence threshold/durations/peak hold). Used by validator-only
// tests where we exercise a specific check; the rest stays at zero defaults
// because the bodies under test don't touch them.
func minimalValidUpdate() *SettingsUpdate {
	return &SettingsUpdate{
		SilenceThreshold:  -40,
		SilenceDurationMs: 15000,
		SilenceRecoveryMs: 5000,
		PeakHoldMs:        1500,
	}
}

// TestSettingsUpdateValidate_ClearGraphSecretConflict pins #247: when both
// ClearGraphClientSecret=true and a non-empty GraphClientSecret are submitted,
// Validate() reports the conflict. Raw != "" — TrimSpace is NOT applied here.
func TestSettingsUpdateValidate_ClearGraphSecretConflict(t *testing.T) {
	t.Parallel()

	upd := minimalValidUpdate()
	upd.GraphClientSecret = "new-secret"
	upd.ClearGraphClientSecret = true

	errs := upd.Validate()
	want := "clear_graph_client_secret: conflicts with non-empty graph_client_secret"
	found := false
	for _, e := range errs {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Validate() = %v, want to contain %q", errs, want)
	}
}

// TestSettingsUpdateValidate_ClearGraphSecretWithBlankAllowed pins that an
// empty submitted secret combined with ClearGraphClientSecret=true is NOT a
// conflict (this is the supported "remove saved secret" path).
func TestSettingsUpdateValidate_ClearGraphSecretWithBlankAllowed(t *testing.T) {
	t.Parallel()

	upd := minimalValidUpdate()
	upd.GraphClientSecret = ""
	upd.ClearGraphClientSecret = true

	for _, e := range upd.Validate() {
		if e == "clear_graph_client_secret: conflicts with non-empty graph_client_secret" {
			t.Fatalf("Validate() unexpectedly reported conflict for empty secret + clear=true: %q", e)
		}
	}
}

// TestSettingsUpdateValidate_ClearWhatsAppTokenConflict mirrors the Graph
// conflict test for the WhatsApp access token.
func TestSettingsUpdateValidate_ClearWhatsAppTokenConflict(t *testing.T) {
	t.Parallel()

	upd := minimalValidUpdate()
	upd.WhatsAppAccessToken = "new-token"
	upd.ClearWhatsAppAccessToken = true

	errs := upd.Validate()
	want := "clear_whatsapp_access_token: conflicts with non-empty whatsapp_access_token"
	found := false
	for _, e := range errs {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Validate() = %v, want to contain %q", errs, want)
	}
}

// TestSettingsUpdateValidate_ClearWhatsAppTokenWithBlankAllowed pins that an
// empty submitted token combined with ClearWhatsAppAccessToken=true is NOT a
// conflict.
func TestSettingsUpdateValidate_ClearWhatsAppTokenWithBlankAllowed(t *testing.T) {
	t.Parallel()

	upd := minimalValidUpdate()
	upd.WhatsAppAccessToken = ""
	upd.ClearWhatsAppAccessToken = true

	for _, e := range upd.Validate() {
		if e == "clear_whatsapp_access_token: conflicts with non-empty whatsapp_access_token" {
			t.Fatalf("Validate() unexpectedly reported conflict for empty token + clear=true: %q", e)
		}
	}
}
