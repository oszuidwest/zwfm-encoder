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

	reloaded := New(configPath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}

	reloadedSnap := reloaded.Snapshot()
	assertDefaultScalarSettings(t, &reloadedSnap)
}

func TestLoadAcceptsDocumentedMinimalConfig(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "system": {
    "port": 8080,
    "username": "admin",
    "password": "encoder"
  },
  "web": {
    "station_name": "ZuidWest FM"
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v, want nil for documented minimal config", err)
	}

	snap := cfg.Snapshot()
	assertDefaultScalarSettings(t, &snap)
}

func TestLoadDefaultsDoNotOverrideConfiguredValues(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "system": {
    "port": 9090,
    "username": "operator",
    "password": "secret"
  },
  "web": {
    "station_name": "Custom Station",
    "color_light": "#123456"
  },
  "silence_detection": {
    "threshold_db": -35,
    "duration_ms": 20000
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
	assertEqual(t, "WebPort", snap.WebPort, 9090)
	assertEqual(t, "WebUser", snap.WebUser, "operator")
	assertEqual(t, "WebPassword", snap.WebPassword, "secret")
	assertEqual(t, "StationName", snap.StationName, "Custom Station")
	assertEqual(t, "StationColorLight", snap.StationColorLight, "#123456")
	assertEqual(t, "StationColorDark", snap.StationColorDark, DefaultStationColorDark)
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, -35.0)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, int64(20000))
	// Literal assertions guard against accidentally swapping default constants
	// in applyOptionalDefaults (e.g. RecoveryMs <-> PeakHoldMs).
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, int64(5000))
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, int64(3000))
}

func TestLoadKeepsSilenceDefaultsForPartialBlock(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "system": {"port": 8080, "username": "admin", "password": "encoder"},
  "web": {"station_name": "ZuidWest FM"},
  "silence_detection": {"threshold_db": -35}
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	snap := cfg.Snapshot()
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, -35.0)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, DefaultSilenceDurationMs)
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, DefaultSilenceRecoveryMs)
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, DefaultPeakHoldMs)
}

func TestLoadDefaultsChannelImbalanceWhenBlockMissing(t *testing.T) {
	t.Parallel()

	// validConfigJSON("") omits the block, like pre-feature configs.
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(validConfigJSON("")), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	snap := cfg.Snapshot()
	assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, DefaultChannelImbalanceThreshold)
	assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, DefaultChannelImbalanceDurationMs)
	assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, DefaultChannelImbalanceRecoveryMs)
}

func TestLoadKeepsChannelImbalanceDefaultsForEmptyAndPartialBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		block         string
		wantThreshold float64
		wantDuration  int64
		wantRecovery  int64
	}{
		{
			name:          "empty object keeps all defaults",
			block:         `"channel_imbalance_detection":{}`,
			wantThreshold: DefaultChannelImbalanceThreshold,
			wantDuration:  DefaultChannelImbalanceDurationMs,
			wantRecovery:  DefaultChannelImbalanceRecoveryMs,
		},
		{
			name:          "partial block only overrides present fields",
			block:         `"channel_imbalance_detection":{"threshold_db":20}`,
			wantThreshold: 20,
			wantDuration:  DefaultChannelImbalanceDurationMs,
			wantRecovery:  DefaultChannelImbalanceRecoveryMs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(validConfigJSON(tt.block)), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg := New(configPath)
			if err := cfg.Load(); err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			snap := cfg.Snapshot()
			assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, tt.wantThreshold)
			assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, tt.wantDuration)
			assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, tt.wantRecovery)
		})
	}
}

func TestLoadAcceptsEmptyWebBlock(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "system": {"port": 8080, "username": "admin", "password": "encoder"},
  "web": {}
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v, want nil for empty web block", err)
	}

	snap := cfg.Snapshot()
	assertEqual(t, "StationName", snap.StationName, DefaultStationName)
	assertEqual(t, "StationColorLight", snap.StationColorLight, DefaultStationColorLight)
	assertEqual(t, "StationColorDark", snap.StationColorDark, DefaultStationColorDark)
}

func TestLoadPreservesConfigDataAndSnapshotEntities(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	want := fullyPopulatedConfigData(t)
	data, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !reflect.DeepEqual(cfg.ConfigData, want) {
		t.Fatalf("ConfigData after Load() mismatch:\n got  %+v\n want %+v", cfg.ConfigData, want)
	}

	snap := cfg.Snapshot()
	if !reflect.DeepEqual(snap.Streams, want.Streaming.Streams) {
		t.Fatalf("Snapshot().Streams = %+v, want %+v", snap.Streams, want.Streaming.Streams)
	}
	if !reflect.DeepEqual(snap.Recorders, want.Recording.Recorders) {
		t.Fatalf("Snapshot().Recorders = %+v, want %+v", snap.Recorders, want.Recording.Recorders)
	}
	if snap.AudioInput != want.Audio.Input {
		t.Fatalf("Snapshot().AudioInput = %q, want %q", snap.AudioInput, want.Audio.Input)
	}
	if snap.RecordingAPIKey != want.Recording.APIKey {
		t.Fatalf("Snapshot().RecordingAPIKey = %q, want %q", snap.RecordingAPIKey, want.Recording.APIKey)
	}
	if snap.WebhookEvents != want.Notifications.Webhook.Events {
		t.Fatalf("Snapshot().WebhookEvents = %+v, want %+v", snap.WebhookEvents, want.Notifications.Webhook.Events)
	}
}

func TestLoadRejectsInvalidConfiguredEntities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "stream missing id",
			data:    validConfigJSON(`"streaming":{"streams":[{"host":"127.0.0.1","port":9000,"codec":"pcm"}]}`),
			wantErr: "invalid streaming.streams[0].id: is required",
		},
		{
			name:    "stream whitespace id",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"  ","host":"127.0.0.1","port":9000,"codec":"pcm"}]}`),
			wantErr: "invalid streaming.streams[0].id: is required",
		},
		{
			name:    "stream missing codec",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000}]}`),
			wantErr: "invalid streaming.streams[0]: codec: must be pcm, mp3, or opus",
		},
		{
			name:    "duplicate stream id",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"pcm"},{"id":"stream-1","host":"127.0.0.1","port":9001,"codec":"pcm"}]}`),
			wantErr: `invalid streaming.streams[1].id: duplicate "stream-1" also used by streaming.streams[0]`,
		},
		{
			name:    "duplicate stream id points to first occurrence",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"pcm"},{"id":"stream-2","host":"127.0.0.1","port":9001,"codec":"pcm"},{"id":"stream-1","host":"127.0.0.1","port":9002,"codec":"pcm"}]}`),
			wantErr: `invalid streaming.streams[2].id: duplicate "stream-1" also used by streaming.streams[0]`,
		},
		{
			name:    "duplicate enabled listener bind",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","mode":"listener","host":"","port":9000,"codec":"mp3","enabled":true},{"id":"stream-2","mode":"listener","host":"0.0.0.0","port":9000,"codec":"mp3","enabled":true}]}`),
			wantErr: `invalid streaming.streams[1]: duplicate listener bind "0.0.0.0:9000" also used by streaming.streams[0]`,
		},
		{
			name:    "listener stream id rejected",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","mode":"listener","host":"","port":9000,"stream_id":"studio","codec":"mp3"}]}`),
			wantErr: `invalid streaming.streams[0]: stream_id: not supported for listener mode`,
		},
		{
			name:    "short stream password rejected",
			data:    validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"mp3","password":"short"}]}`),
			wantErr: `invalid streaming.streams[0]: password: must be empty or between 10 and 64 characters`,
		},
		{
			name:    "recorder missing id",
			data:    validConfigJSON(`"recording":{"recorders":[{"name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: "invalid recording.recorders[0].id: is required",
		},
		{
			name:    "recorder whitespace id",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"  ","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: "invalid recording.recorders[0].id: is required",
		},
		{
			name:    "recorder missing codec",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: "invalid recording.recorders[0]: codec: must be pcm, mp3, or opus",
		},
		{
			name:    "recorder missing recording mode",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: "invalid recording.recorders[0]: recording_mode: must be hourly or ondemand",
		},
		{
			name:    "recorder missing storage mode",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","local_path":"/tmp"}]}`),
			wantErr: "invalid recording.recorders[0]: storage_mode: must be local, s3, or both",
		},
		{
			name:    "duplicate recorder id",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-1","name":"Backup","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: `invalid recording.recorders[1].id: duplicate "recorder-1" also used by recording.recorders[0]`,
		},
		{
			name:    "duplicate recorder id points to first occurrence",
			data:    validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-2","name":"Backup","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-1","name":"Aux","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`),
			wantErr: `invalid recording.recorders[2].id: duplicate "recorder-1" also used by recording.recorders[0]`,
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

func TestLoadNormalizesListenerHostAndAllowsCallerSharedRemote(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := validConfigJSON(`"streaming":{"streams":[
		{"id":"listener-1","mode":"listener","host":"","port":9000,"codec":"mp3","enabled":true},
		{"id":"caller-1","mode":"caller","host":"stream.example.com","port":9000,"codec":"mp3","enabled":true},
		{"id":"caller-2","mode":"caller","host":"stream.example.com","port":9000,"codec":"mp3","enabled":true}
	]}`)
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	streams := cfg.ConfiguredStreams()
	if got := streams[0].Host; got != types.DefaultListenerBindHost {
		t.Fatalf("listener host = %q, want %q", got, types.DefaultListenerBindHost)
	}
}

func assertDefaultScalarSettings(t *testing.T, snap *Snapshot) {
	t.Helper()

	assertEqual(t, "WebPort", snap.WebPort, DefaultWebPort)
	assertEqual(t, "StationName", snap.StationName, DefaultStationName)
	assertEqual(t, "StationColorLight", snap.StationColorLight, DefaultStationColorLight)
	assertEqual(t, "StationColorDark", snap.StationColorDark, DefaultStationColorDark)
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, DefaultSilenceThreshold)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, DefaultSilenceDurationMs)
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, DefaultSilenceRecoveryMs)
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, DefaultPeakHoldMs)
	assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, DefaultChannelImbalanceThreshold)
	assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, DefaultChannelImbalanceDurationMs)
	assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, DefaultChannelImbalanceRecoveryMs)
	assertEqual(t, "SilenceDumpEnabled", snap.SilenceDumpEnabled, false)
	assertEqual(t, "SilenceDumpRetentionDays", snap.SilenceDumpRetentionDays, 0)
	assertEqual(t, "ZabbixPort", snap.ZabbixPort, 0)
	assertEqual(t, "RecordingMaxDurationMinutes", snap.RecordingMaxDurationMinutes, 0)
	assertEqual(t, "WebhookEvents", snap.WebhookEvents, types.EventSubscriptions{})
	assertEqual(t, "EmailEvents", snap.EmailEvents, types.EventSubscriptions{})
	assertEqual(t, "ZabbixEvents", snap.ZabbixEvents, types.EventSubscriptions{})
}

func fullyPopulatedConfigData(t *testing.T) ConfigData {
	t.Helper()

	return ConfigData{
		System: SystemConfig{
			FFmpegPath: "/usr/bin/ffmpeg",
			Port:       8090,
			Username:   "operator",
			Password:   "secret",
		},
		Web: WebConfig{
			StationName: "ZuidWest Test",
			ColorLight:  "#123456",
			ColorDark:   "#654321",
		},
		Audio: AudioConfig{
			Input: "hw:1,0",
		},
		SilenceDetection: SilenceDetectionConfig{
			ThresholdDB: -35.5,
			DurationMs:  12000,
			RecoveryMs:  4000,
			PeakHoldMs:  2500,
		},
		ChannelImbalanceDetection: ChannelImbalanceDetectionConfig{
			ThresholdDB: 10,
			DurationMs:  10000,
			RecoveryMs:  3000,
		},
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       true,
			RetentionDays: 3,
		},
		Notifications: NotificationsConfig{
			Webhook: WebhookConfig{
				URL: "https://example.com/hook",
				Events: types.EventSubscriptions{
					SilenceStart: true,
					AudioDump:    true,
				},
			},
			Email: EmailConfig{
				TenantID:     "tenant",
				ClientID:     "client",
				ClientSecret: "email-secret",
				FromAddress:  "studio@example.com",
				Recipients:   "ops@example.com, engineer@example.com",
				Events: types.EventSubscriptions{
					SilenceEnd: true,
					AudioDump:  true,
				},
			},
			Zabbix: types.ZabbixConfig{
				Server:     "zabbix.example.com",
				Port:       10051,
				Host:       "encoder-1",
				SilenceKey: "encoder.silence",
				UploadKey:  "encoder.upload",
				Events: types.ZabbixEventSubscriptions{
					SilenceStart: true,
					SilenceEnd:   true,
				},
			},
		},
		Streaming: StreamingConfig{
			Streams: []types.Stream{
				{
					ID:         "stream-1",
					Enabled:    true,
					Host:       "srt.example.com",
					Port:       9000,
					Password:   "stream-secret",
					StreamID:   "station/main",
					Codec:      types.CodecMP3,
					Bitrate:    192,
					MaxRetries: 5,
					CreatedAt:  1700000000000,
				},
			},
		},
		Recording: RecordingConfig{
			APIKey:             "recording-key",
			MaxDurationMinutes: 90,
			Recorders: []types.Recorder{
				{
					ID:                "recorder-1",
					Name:              "Archive",
					Enabled:           true,
					Codec:             types.CodecOpus,
					Bitrate:           128,
					RecordingMode:     types.RecordingOnDemand,
					StorageMode:       types.StorageBoth,
					LocalPath:         "/var/archive",
					S3Endpoint:        "https://s3.example.com",
					S3Bucket:          "archive",
					S3AccessKeyID:     "access-key",
					S3SecretAccessKey: "secret-key",
					RetentionDays:     14,
					CreatedAt:         1700000001000,
				},
			},
		},
	}
}

func assertEqual[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()

	if got != want {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func validConfigJSON(extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	raw := `{
		"system": {"port": 8080, "username": "admin", "password": "encoder"},
		"web": {"station_name": "ZuidWest FM", "color_light": "#E6007E", "color_dark": "#E6007E"},
		"silence_detection": {"threshold_db": -40, "duration_ms": 15000, "recovery_ms": 5000, "peak_hold_ms": 3000}
	` + extra + `}`
	// Roundtrip through a map so any section in `extra` overrides the base
	// instead of producing a duplicate top-level key (rejected by Load).
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic("validConfigJSON: invalid JSON fragment: " + err.Error())
	}
	out, err := json.Marshal(m)
	if err != nil {
		panic("validConfigJSON: re-marshal failed: " + err.Error())
	}
	return string(out)
}

func TestLoadPreservesExplicitZeroAndFalseValues(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(validConfigJSON(`
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
      "events": {"silence_start": false, "silence_end": false}
	    }
	  }
	`))
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
	if snap.ZabbixEvents != (types.EventSubscriptions{}) {
		t.Fatalf("ZabbixEvents = %+v, want both false", snap.ZabbixEvents)
	}
	if snap.ZabbixPort != 0 {
		t.Fatalf("ZabbixPort = %d, want 0", snap.ZabbixPort)
	}
	if snap.WebPort != DefaultWebPort {
		t.Fatalf("WebPort = %d, want explicit %d", snap.WebPort, DefaultWebPort)
	}
	if snap.RecordingMaxDurationMinutes != 0 {
		t.Fatalf("RecordingMaxDurationMinutes = %d, want zero value", snap.RecordingMaxDurationMinutes)
	}
}

func TestZabbixEventsJSONSemantics(t *testing.T) {
	t.Parallel()

	allFalse := types.EventSubscriptions{}

	tests := []struct {
		name string
		json string
		want types.EventSubscriptions
	}{
		{
			name: "missing events field stays empty",
			json: validConfigJSON(`"notifications":{"zabbix":{}}`),
			want: allFalse,
		},
		{
			name: "events null stays empty",
			json: validConfigJSON(`"notifications":{"zabbix":{"events":null}}`),
			want: allFalse,
		},
		{
			name: "events empty object stays empty",
			json: validConfigJSON(`"notifications":{"zabbix":{"events":{}}}`),
			want: allFalse,
		},
		{
			name: "partial object only overrides present fields",
			json: validConfigJSON(`"notifications":{"zabbix":{"events":{"silence_start":true}}}`),
			want: types.EventSubscriptions{SilenceStart: true},
		},
		{
			name: "explicit false values disable events",
			json: validConfigJSON(`"notifications":{"zabbix":{"events":{"silence_start":false,"silence_end":false}}}`),
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

	// Load a config with explicit Zabbix events. Then trigger an unrelated
	// save via AddStream (which calls saveLocked directly, bypassing
	// ApplySettings) and verify event settings survive reload.
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(validConfigJSON(`"notifications":{"zabbix":{"events":{"silence_start":true,"silence_end":true}}}`)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Trigger saveLocked via AddStream without touching settings.
	stream := &types.Stream{Host: "127.0.0.1", Port: 1234, Codec: types.CodecPCM}
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
			name:    "missing system port",
			data:    `{"system":{"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: "system.port: must be between 1 and 65535",
		},
		{
			name:    "missing system username",
			data:    `{"system":{"port":8080,"password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: "system.username: is required",
		},
		{
			name:    "missing system password",
			data:    `{"system":{"port":8080,"username":"admin"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: "system.password: is required",
		},
		{
			name:    "empty station name",
			data:    validConfigJSON(`"web":{"station_name":"","color_light":"#E6007E","color_dark":"#E6007E"}`),
			wantErr: `invalid station_name "": must be 1-30 printable characters`,
		},
		{
			name:    "explicit null station color light",
			data:    `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":null,"color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: `color_light "": must be hex format (#RRGGBB)`,
		},
		{
			name:    "explicit null silence duration",
			data:    `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":null,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: "silence_detection.duration_ms: must be greater than 0",
		},
		{
			name:    "explicit null web block",
			data:    `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":null,"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: `invalid station_name "": must be 1-30 printable characters`,
		},
		{
			name:    "duplicate top-level web key with null override",
			data:    `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000},"web":null}`,
			wantErr: `duplicate key "web"`,
		},
		{
			name:    "duplicate nested color_light key with null override",
			data:    `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E","color_light":null},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`,
			wantErr: `duplicate key "color_light"`,
		},
		{
			name:    "empty station color light",
			data:    validConfigJSON(`"web":{"station_name":"ZuidWest FM","color_light":"","color_dark":"#E6007E"}`),
			wantErr: `color_light "": must be hex format (#RRGGBB)`,
		},
		{
			name:    "empty station color dark",
			data:    validConfigJSON(`"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":""}`),
			wantErr: `color_dark "": must be hex format (#RRGGBB)`,
		},
		{
			name:    "positive silence threshold",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":1,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "zero silence threshold",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":0,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "fractional silence threshold above -1",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":-0.5,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "silence threshold below -60",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":-61,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.threshold_db: must be between -60 and -1 dB",
		},
		{
			name:    "zero silence duration",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":0,"recovery_ms":5000,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.duration_ms: must be greater than 0",
		},
		{
			name:    "zero silence recovery",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":0,"peak_hold_ms":3000}`),
			wantErr: "silence_detection.recovery_ms: must be greater than 0",
		},
		{
			name:    "zero peak hold",
			data:    validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":0}`),
			wantErr: "silence_detection.peak_hold_ms: must be between 500 and 10000 ms",
		},
		{
			name:    "zero channel imbalance threshold",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":0,"duration_ms":15000,"recovery_ms":5000}`),
			wantErr: "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB",
		},
		{
			name:    "channel imbalance threshold at 60 is rejected",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":60,"duration_ms":15000,"recovery_ms":5000}`),
			wantErr: "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB",
		},
		{
			name:    "channel imbalance threshold above 60 is rejected",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":61,"duration_ms":15000,"recovery_ms":5000}`),
			wantErr: "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB",
		},
		{
			name:    "zero channel imbalance duration",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":0,"recovery_ms":5000}`),
			wantErr: "channel_imbalance_detection.duration_ms: must be greater than 0",
		},
		{
			name:    "zero channel imbalance recovery",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":15000,"recovery_ms":0}`),
			wantErr: "channel_imbalance_detection.recovery_ms: must be greater than 0",
		},
		{
			// Explicit null is rejected, matching silence_detection semantics.
			name:    "explicit null channel imbalance duration",
			data:    validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":null,"recovery_ms":5000}`),
			wantErr: "channel_imbalance_detection.duration_ms: must be greater than 0",
		},
		{
			name:    "negative silence dump retention",
			data:    validConfigJSON(`"silence_dump":{"retention_days":-1}`),
			wantErr: "silence_dump.retention_days: cannot be negative",
		},
		{
			name:    "invalid webhook url",
			data:    validConfigJSON(`"notifications":{"webhook":{"url":"://broken"}}`),
			wantErr: "notifications.webhook.url: invalid URL format",
		},
		{
			name:    "invalid graph from address",
			data:    validConfigJSON(`"notifications":{"email":{"from_address":"not-an-email"}}`),
			wantErr: "notifications.email.from_address: invalid email format",
		},
		{
			name:    "invalid graph recipient",
			data:    validConfigJSON(`"notifications":{"email":{"recipients":"good@example.com, bad-address"}}`),
			wantErr: "notifications.email.recipients: contains invalid email address",
		},
		{
			name:    "invalid zabbix port",
			data:    validConfigJSON(`"notifications":{"zabbix":{"port":70000}}`),
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
		if err := os.WriteFile(configPath, []byte(validConfigJSON(`"silence_detection":{"threshold_db":-1,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`)), 0o600); err != nil {
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
		if err := os.WriteFile(configPath, []byte(validConfigJSON(`"silence_detection":{"threshold_db":-60,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`)), 0o600); err != nil {
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

// minimalValidUpdate returns a SettingsUpdate with required range fields filled.
// Other fields stay at zero defaults for focused validation tests.
func minimalValidUpdate() *SettingsUpdate {
	return &SettingsUpdate{
		SilenceThreshold:           -40,
		SilenceDurationMs:          15000,
		SilenceRecoveryMs:          5000,
		PeakHoldMs:                 1500,
		ChannelImbalanceThreshold:  12,
		ChannelImbalanceDurationMs: 15000,
		ChannelImbalanceRecoveryMs: 5000,
	}
}

func TestChannelImbalanceThresholdBoundary(t *testing.T) {
	t.Parallel()

	fileTests := []struct {
		name      string
		threshold string
		wantErr   bool
	}{
		{"one is valid", "1", false},
		{"just below sixty is valid", "59.9", false},
		{"zero is rejected", "0", true},
		{"sixty is rejected", "60", true},
		{"above sixty is rejected", "61", true},
	}
	for _, tt := range fileTests {
		t.Run("file/"+tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.json")
			block := `"channel_imbalance_detection":{"threshold_db":` + tt.threshold + `,"duration_ms":15000,"recovery_ms":5000}`
			if err := os.WriteFile(configPath, []byte(validConfigJSON(block)), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			err := New(configPath).Load()
			if tt.wantErr && err == nil {
				t.Fatalf("Load() error = nil, want rejection for threshold %s", tt.threshold)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Load() error = %v, want nil for threshold %s", err, tt.threshold)
			}
		})
	}

	apiTests := []struct {
		name      string
		threshold float64
		wantErr   bool
	}{
		{"one is valid", 1, false},
		{"just below sixty is valid", 59.9, false},
		{"zero is rejected", 0, true},
		{"sixty is rejected", 60, true},
		{"above sixty is rejected", 61, true},
	}
	for _, tt := range apiTests {
		t.Run("api/"+tt.name, func(t *testing.T) {
			t.Parallel()

			upd := minimalValidUpdate()
			upd.ChannelImbalanceThreshold = tt.threshold

			var found bool
			for _, e := range upd.Validate() {
				if strings.Contains(e, "channel_imbalance_threshold") {
					found = true
					break
				}
			}
			if found != tt.wantErr {
				t.Fatalf("Validate() reported channel_imbalance_threshold error = %v, want %v for %v", found, tt.wantErr, tt.threshold)
			}
		})
	}
}

func TestApplySettingsRoundTripsChannelImbalance(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := New(configPath)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	upd := minimalValidUpdate()
	upd.ChannelImbalanceThreshold = 18
	upd.ChannelImbalanceDurationMs = 20000
	upd.ChannelImbalanceRecoveryMs = 4000
	if err := cfg.ApplySettings(upd); err != nil {
		t.Fatalf("ApplySettings() error = %v", err)
	}

	reloaded := New(configPath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}

	snap := reloaded.Snapshot()
	assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, 18.0)
	assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, int64(20000))
	assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, int64(4000))
}

// TestSettingsUpdateValidate_ClearGraphSecretConflict pins #247: when both
// ClearGraphClientSecret=true and a non-empty GraphClientSecret are submitted,
// Validate() reports the conflict. The whitespace-only case is included to
// pin that the check uses raw != "" rather than TrimSpace; if the contract
// ever shifts to "semantically empty == empty" this case will fail clearly.
func TestSettingsUpdateValidate_ClearGraphSecretConflict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		secret string
	}{
		{"plain non-empty", "new-secret"},
		{"whitespace-only (raw != \"\" still triggers conflict)", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upd := minimalValidUpdate()
			upd.GraphClientSecret = tc.secret
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
		})
	}
}

// TestSettingsUpdateValidate_ClearGraphSecretWithBlankAllowed pins that an
// empty submitted secret combined with ClearGraphClientSecret=true is not a
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

func TestSettingsUpdateValidate_ClearWebhookURLConflict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"plain non-empty", "https://hooks.example.com/new-token"},
		{"whitespace-only (raw != \"\" still triggers conflict)", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upd := minimalValidUpdate()
			upd.WebhookURL = tc.url
			upd.ClearWebhookURL = true

			errs := upd.Validate()
			want := "clear_webhook_url: conflicts with non-empty webhook_url"
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
		})
	}
}

func TestSettingsUpdateValidate_ClearWebhookURLWithBlankAllowed(t *testing.T) {
	t.Parallel()

	upd := minimalValidUpdate()
	upd.WebhookURL = ""
	upd.ClearWebhookURL = true

	for _, e := range upd.Validate() {
		if e == "clear_webhook_url: conflicts with non-empty webhook_url" {
			t.Fatalf("Validate() unexpectedly reported conflict for empty URL + clear=true: %q", e)
		}
	}
}
