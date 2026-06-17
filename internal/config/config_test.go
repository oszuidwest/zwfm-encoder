package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return configPath
}
func mustLoad(t *testing.T, cfg *Config) {
	t.Helper()
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
func newLoadedConfig(t *testing.T) (cfg *Config, configPath string) {
	t.Helper()
	configPath = filepath.Join(t.TempDir(), "config.json")
	cfg = New(configPath)
	mustLoad(t, cfg)
	return cfg, configPath
}
func loadConfig(t *testing.T, data string) *Config {
	t.Helper()
	cfg := New(writeConfig(t, data))
	mustLoad(t, cfg)
	return cfg
}
func loadSnapshot(t *testing.T, data string) Snapshot {
	t.Helper()
	return loadConfig(t, data).Snapshot()
}
func assertLoadErrorContains(t *testing.T, data, want string) {
	t.Helper()
	err := New(writeConfig(t, data)).Load()
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Load() error = %q, want substring %q", err.Error(), want)
	}
}
func hasValidationError(errs []string, want string) bool {
	for _, err := range errs {
		if strings.Contains(err, want) {
			return true
		}
	}
	return false
}

// invalidLoadCase describes one invalid config load fixture.
type invalidLoadCase struct {
	name    string
	data    string
	wantErr string
}

// badConfig builds an invalid load fixture with its expected validation substring.
func badConfig(name, data, wantErr string) invalidLoadCase {
	return invalidLoadCase{name: name, data: data, wantErr: wantErr}
}
func TestLoadCreatesDefaultConfig(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := New(configPath)
	mustLoad(t, cfg)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}
	snap := cfg.Snapshot()
	assertDefaultScalarSettings(t, &snap)
	reloaded := New(configPath)
	mustLoad(t, reloaded)
	reloadedSnap := reloaded.Snapshot()
	assertDefaultScalarSettings(t, &reloadedSnap)
}
func TestLoadAcceptsDocumentedMinimalConfig(t *testing.T) {
	t.Parallel()
	snap := loadSnapshot(t, `{
  "system": {
    "port": 8080,
    "username": "admin",
    "password": "encoder"
  },
  "web": {
    "station_name": "ZuidWest FM"
  }
}`)
	assertDefaultScalarSettings(t, &snap)
}
func TestLoadDefaultsDoNotOverrideConfiguredValues(t *testing.T) {
	t.Parallel()
	snap := loadSnapshot(t, `{
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
	assertEqual(t, "WebPort", snap.WebPort, 9090)
	assertEqual(t, "WebUser", snap.WebUser, "operator")
	assertEqual(t, "WebPassword", snap.WebPassword, "secret")
	assertEqual(t, "StationName", snap.StationName, "Custom Station")
	assertEqual(t, "StationColorLight", snap.StationColorLight, "#123456")
	assertEqual(t, "StationColorDark", snap.StationColorDark, DefaultStationColorDark)
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, -35.0)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, int64(20000))
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, int64(5000))
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, int64(3000))
}
func TestLoadKeepsSilenceDefaultsForPartialBlock(t *testing.T) {
	t.Parallel()
	snap := loadSnapshot(t, `{
  "system": {"port": 8080, "username": "admin", "password": "encoder"},
  "web": {"station_name": "ZuidWest FM"},
  "silence_detection": {"threshold_db": -35}
}`)
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, -35.0)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, DefaultSilenceDurationMs)
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, DefaultSilenceRecoveryMs)
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, DefaultPeakHoldMs)
}
func TestLoadDefaultsChannelImbalanceWhenBlockMissing(t *testing.T) {
	t.Parallel()
	snap := loadSnapshot(t, validConfigJSON(""))
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
			snap := loadSnapshot(t, validConfigJSON(tt.block))
			assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, tt.wantThreshold)
			assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, tt.wantDuration)
			assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, tt.wantRecovery)
		})
	}
}
func TestLoadAcceptsEmptyWebBlock(t *testing.T) {
	t.Parallel()
	snap := loadSnapshot(t, `{
  "system": {"port": 8080, "username": "admin", "password": "encoder"},
  "web": {}
}`)
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
	tests := []invalidLoadCase{
		badConfig("stream missing id", validConfigJSON(`"streaming":{"streams":[{"host":"127.0.0.1","port":9000,"codec":"pcm"}]}`), "invalid streaming.streams[0].id: is required"),
		badConfig("stream whitespace id", validConfigJSON(`"streaming":{"streams":[{"id":"  ","host":"127.0.0.1","port":9000,"codec":"pcm"}]}`), "invalid streaming.streams[0].id: is required"),
		badConfig("stream missing codec", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000}]}`), "invalid streaming.streams[0]: codec: must be pcm, mp3, or opus"),
		badConfig("duplicate stream id", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"pcm"},{"id":"stream-1","host":"127.0.0.1","port":9001,"codec":"pcm"}]}`), `invalid streaming.streams[1].id: duplicate "stream-1" also used by streaming.streams[0]`),
		badConfig("duplicate stream id points to first occurrence", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"pcm"},{"id":"stream-2","host":"127.0.0.1","port":9001,"codec":"pcm"},{"id":"stream-1","host":"127.0.0.1","port":9002,"codec":"pcm"}]}`), `invalid streaming.streams[2].id: duplicate "stream-1" also used by streaming.streams[0]`),
		badConfig("duplicate enabled listener bind", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","mode":"listener","host":"","port":9000,"codec":"mp3","enabled":true},{"id":"stream-2","mode":"listener","host":"0.0.0.0","port":9000,"codec":"mp3","enabled":true}]}`), `invalid streaming.streams[1]: duplicate listener bind "0.0.0.0:9000" also used by streaming.streams[0]`),
		badConfig("listener stream id rejected", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","mode":"listener","host":"","port":9000,"stream_id":"studio","codec":"mp3"}]}`), `invalid streaming.streams[0]: stream_id: not supported for listener mode`),
		badConfig("short stream password rejected", validConfigJSON(`"streaming":{"streams":[{"id":"stream-1","host":"127.0.0.1","port":9000,"codec":"mp3","password":"short"}]}`), `invalid streaming.streams[0]: password: must be empty or between 10 and 64 characters`),
		badConfig("recorder missing id", validConfigJSON(`"recording":{"recorders":[{"name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`), "invalid recording.recorders[0].id: is required"),
		badConfig("recorder whitespace id", validConfigJSON(`"recording":{"recorders":[{"id":"  ","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`), "invalid recording.recorders[0].id: is required"),
		badConfig("recorder missing codec", validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`), "invalid recording.recorders[0]: codec: must be pcm, mp3, or opus"),
		badConfig("recorder missing recording mode", validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","storage_mode":"local","local_path":"/tmp"}]}`), "invalid recording.recorders[0]: recording_mode: must be hourly or ondemand"),
		badConfig("recorder missing storage mode", validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","local_path":"/tmp"}]}`), "invalid recording.recorders[0]: storage_mode: must be local, s3, or both"),
		badConfig("duplicate recorder id", validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-1","name":"Backup","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`), `invalid recording.recorders[1].id: duplicate "recorder-1" also used by recording.recorders[0]`),
		badConfig("duplicate recorder id points to first occurrence", validConfigJSON(`"recording":{"recorders":[{"id":"recorder-1","name":"Archive","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-2","name":"Backup","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"},{"id":"recorder-1","name":"Aux","codec":"pcm","recording_mode":"hourly","storage_mode":"local","local_path":"/tmp"}]}`), `invalid recording.recorders[2].id: duplicate "recorder-1" also used by recording.recorders[0]`),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertLoadErrorContains(t, tt.data, tt.wantErr)
		})
	}
}
func TestLoadNormalizesListenerHostAndAllowsCallerSharedRemote(t *testing.T) {
	t.Parallel()
	data := validConfigJSON(`"streaming":{"streams":[
		{"id":"listener-1","mode":"listener","host":"","port":9000,"codec":"mp3","enabled":true},
		{"id":"caller-1","mode":"caller","host":"stream.example.com","port":9000,"codec":"mp3","enabled":true},
		{"id":"caller-2","mode":"caller","host":"stream.example.com","port":9000,"codec":"mp3","enabled":true}
	]}`)
	cfg := loadConfig(t, data)
	streams := cfg.ConfiguredStreams()
	if got := streams[0].Host; got != types.DefaultListenerBindHost {
		t.Fatalf("listener host = %q, want %q", got, types.DefaultListenerBindHost)
	}
}
func TestAddStreamRollsBackDuplicateListenerBind(t *testing.T) {
	t.Parallel()
	cfg := New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Streaming.Streams = []types.Stream{listenerStream("listener-1", 9000)}
	err := cfg.AddStream(&types.Stream{Enabled: true, Mode: types.StreamModeListener, Host: types.DefaultListenerBindHost, Port: 9000, Codec: types.CodecMP3})
	if !errors.Is(err, ErrInvalidStreamConfig) {
		t.Fatalf("AddStream() error = %v, want ErrInvalidStreamConfig", err)
	}
	streams := cfg.ConfiguredStreams()
	if len(streams) != 1 {
		t.Fatalf("ConfiguredStreams len = %d, want 1 after rollback: %+v", len(streams), streams)
	}
	if streams[0].ID != "listener-1" {
		t.Fatalf("remaining stream ID = %q, want listener-1", streams[0].ID)
	}
}
func TestUpdateStreamRollsBackDuplicateListenerBind(t *testing.T) {
	t.Parallel()
	cfg := New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Streaming.Streams = []types.Stream{
		listenerStream("listener-1", 9000),
		listenerStream("listener-2", 9001),
	}
	err := cfg.UpdateStream(&types.Stream{ID: "listener-2", Enabled: true, Mode: types.StreamModeListener, Host: types.DefaultListenerBindHost, Port: 9000, Codec: types.CodecMP3})
	if !errors.Is(err, ErrInvalidStreamConfig) {
		t.Fatalf("UpdateStream() error = %v, want ErrInvalidStreamConfig", err)
	}
	stream := cfg.Stream("listener-2")
	if stream == nil {
		t.Fatal("listener-2 disappeared after failed update")
	}
	if stream.Port != 9001 {
		t.Fatalf("listener-2 port = %d, want 9001 after rollback", stream.Port)
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
func listenerStream(id string, port int) types.Stream {
	return types.Stream{
		ID:      id,
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    types.DefaultListenerBindHost,
		Port:    port,
		Codec:   types.CodecMP3,
	}
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
	snap := loadSnapshot(t, validConfigJSON(`
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
	assertEqual(t, "SilenceThreshold", snap.SilenceThreshold, -1.0)
	assertEqual(t, "SilenceDurationMs", snap.SilenceDurationMs, int64(1234))
	assertEqual(t, "SilenceRecoveryMs", snap.SilenceRecoveryMs, int64(4321))
	assertEqual(t, "PeakHoldMs", snap.PeakHoldMs, int64(999))
	if snap.SilenceDumpEnabled {
		t.Fatal("SilenceDumpEnabled = true, want false")
	}
	assertEqual(t, "SilenceDumpRetentionDays", snap.SilenceDumpRetentionDays, 0)
	assertEqual(t, "WebhookEvents", snap.WebhookEvents, types.EventSubscriptions{})
	assertEqual(t, "EmailEvents", snap.EmailEvents, types.EventSubscriptions{})
	assertEqual(t, "ZabbixEvents", snap.ZabbixEvents, types.EventSubscriptions{})
	assertEqual(t, "ZabbixPort", snap.ZabbixPort, 0)
	assertEqual(t, "WebPort", snap.WebPort, DefaultWebPort)
	assertEqual(t, "RecordingMaxDurationMinutes", snap.RecordingMaxDurationMinutes, 0)
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
			got := loadSnapshot(t, tt.json).ZabbixEvents
			if got != tt.want {
				t.Fatalf("ZabbixEvents = %+v, want %+v", got, tt.want)
			}
		})
	}
}
func TestZabbixEventsRoundTrip(t *testing.T) {
	t.Parallel()
	configPath := writeConfig(t, validConfigJSON(`"notifications":{"zabbix":{"events":{"silence_start":true,"silence_end":true}}}`))
	cfg := New(configPath)
	mustLoad(t, cfg)
	stream := &types.Stream{Host: "127.0.0.1", Port: 1234, Codec: types.CodecPCM}
	if err := cfg.AddStream(stream); err != nil {
		t.Fatalf("AddStream() error = %v", err)
	}
	cfg2 := New(configPath)
	mustLoad(t, cfg2)
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
	tests := []invalidLoadCase{
		badConfig("missing system port", `{"system":{"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, "system.port: must be between 1 and 65535"),
		badConfig("missing system username", `{"system":{"port":8080,"password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, "system.username: is required"),
		badConfig("missing system password", `{"system":{"port":8080,"username":"admin"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, "system.password: is required"),
		badConfig("empty station name", validConfigJSON(`"web":{"station_name":"","color_light":"#E6007E","color_dark":"#E6007E"}`), `invalid station_name "": must be 1-30 printable characters`),
		badConfig("explicit null station color light", `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":null,"color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, `color_light "": must be hex format (#RRGGBB)`),
		badConfig("explicit null silence duration", `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":null,"recovery_ms":5000,"peak_hold_ms":3000}}`, "silence_detection.duration_ms: must be greater than 0"),
		badConfig("explicit null web block", `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":null,"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, `invalid station_name "": must be 1-30 printable characters`),
		badConfig("duplicate top-level web key with null override", `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E"},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000},"web":null}`, `duplicate key "web"`),
		badConfig("duplicate nested color_light key with null override", `{"system":{"port":8080,"username":"admin","password":"encoder"},"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":"#E6007E","color_light":null},"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}}`, `duplicate key "color_light"`),
		badConfig("empty station color light", validConfigJSON(`"web":{"station_name":"ZuidWest FM","color_light":"","color_dark":"#E6007E"}`), `color_light "": must be hex format (#RRGGBB)`),
		badConfig("empty station color dark", validConfigJSON(`"web":{"station_name":"ZuidWest FM","color_light":"#E6007E","color_dark":""}`), `color_dark "": must be hex format (#RRGGBB)`),
		badConfig("positive silence threshold", validConfigJSON(`"silence_detection":{"threshold_db":1,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`), "silence_detection.threshold_db: must be between -60 and -1 dB"),
		badConfig("zero silence threshold", validConfigJSON(`"silence_detection":{"threshold_db":0,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`), "silence_detection.threshold_db: must be between -60 and -1 dB"),
		badConfig("fractional silence threshold above -1", validConfigJSON(`"silence_detection":{"threshold_db":-0.5,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`), "silence_detection.threshold_db: must be between -60 and -1 dB"),
		badConfig("silence threshold below -60", validConfigJSON(`"silence_detection":{"threshold_db":-61,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`), "silence_detection.threshold_db: must be between -60 and -1 dB"),
		badConfig("zero silence duration", validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":0,"recovery_ms":5000,"peak_hold_ms":3000}`), "silence_detection.duration_ms: must be greater than 0"),
		badConfig("zero silence recovery", validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":0,"peak_hold_ms":3000}`), "silence_detection.recovery_ms: must be greater than 0"),
		badConfig("zero peak hold", validConfigJSON(`"silence_detection":{"threshold_db":-40,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":0}`), "silence_detection.peak_hold_ms: must be between 500 and 10000 ms"),
		badConfig("zero channel imbalance threshold", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":0,"duration_ms":15000,"recovery_ms":5000}`), "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB"),
		badConfig("channel imbalance threshold at 60 is rejected", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":60,"duration_ms":15000,"recovery_ms":5000}`), "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB"),
		badConfig("channel imbalance threshold above 60 is rejected", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":61,"duration_ms":15000,"recovery_ms":5000}`), "channel_imbalance_detection.threshold_db: must be at least 1 dB and below 60 dB"),
		badConfig("zero channel imbalance duration", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":0,"recovery_ms":5000}`), "channel_imbalance_detection.duration_ms: must be greater than 0"),
		badConfig("zero channel imbalance recovery", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":15000,"recovery_ms":0}`), "channel_imbalance_detection.recovery_ms: must be greater than 0"),
		badConfig("explicit null channel imbalance duration", validConfigJSON(`"channel_imbalance_detection":{"threshold_db":12,"duration_ms":null,"recovery_ms":5000}`), "channel_imbalance_detection.duration_ms: must be greater than 0"),
		badConfig("negative silence dump retention", validConfigJSON(`"silence_dump":{"retention_days":-1}`), "silence_dump.retention_days: cannot be negative"),
		badConfig("invalid webhook url", validConfigJSON(`"notifications":{"webhook":{"url":"://broken"}}`), "notifications.webhook.url: invalid URL format"),
		badConfig("invalid graph from address", validConfigJSON(`"notifications":{"email":{"from_address":"not-an-email"}}`), "notifications.email.from_address: invalid email format"),
		badConfig("invalid graph recipient", validConfigJSON(`"notifications":{"email":{"recipients":"good@example.com, bad-address"}}`), "notifications.email.recipients: contains invalid email address"),
		badConfig("invalid zabbix port", validConfigJSON(`"notifications":{"zabbix":{"port":70000}}`), "notifications.zabbix.port: must be between 1 and 65535"),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertLoadErrorContains(t, tt.data, tt.wantErr)
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
			if !hasValidationError(errs, tt.wantErr) {
				t.Fatalf("Validate() errors = %v, want one containing %q", errs, tt.wantErr)
			}
		})
	}
}
func TestSaveLockedWritePath(t *testing.T) {
	t.Parallel()
	t.Run("permissions are 0600 after save", func(t *testing.T) {
		t.Parallel()
		_, configPath := newLoadedConfig(t)
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
		mustLoad(t, cfg)
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
		_, configPath := newLoadedConfig(t)
		data, err := os.ReadFile(configPath) //nolint:gosec // G304: Test reads a controlled temp path.
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
	for _, threshold := range []float64{-1, -60} {
		t.Run(fmt.Sprintf("file/%g", threshold), func(t *testing.T) {
			t.Parallel()
			block := fmt.Sprintf(`"silence_detection":{"threshold_db":%g,"duration_ms":15000,"recovery_ms":5000,"peak_hold_ms":3000}`, threshold)
			loadConfig(t, validConfigJSON(block))
		})
		t.Run(fmt.Sprintf("api/%g", threshold), func(t *testing.T) {
			t.Parallel()
			update := SettingsUpdate{SilenceThreshold: threshold, SilenceDurationMs: 1, SilenceRecoveryMs: 1}
			if errs := update.Validate(); hasValidationError(errs, "silence_threshold") {
				t.Fatalf("Validate() returned unexpected silence_threshold error: %v", errs)
			}
		})
	}
}

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
	tests := []struct {
		name      string
		threshold string
		value     float64
		wantErr   bool
	}{
		{"one is valid", "1", 1, false},
		{"just below sixty is valid", "59.9", 59.9, false},
		{"zero is rejected", "0", 0, true},
		{"sixty is rejected", "60", 60, true},
		{"above sixty is rejected", "61", 61, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			block := `"channel_imbalance_detection":{"threshold_db":` + tt.threshold + `,"duration_ms":15000,"recovery_ms":5000}`
			fileErr := New(writeConfig(t, validConfigJSON(block))).Load() != nil
			if fileErr != tt.wantErr {
				t.Fatalf("Load() rejected threshold %s = %v, want %v", tt.threshold, fileErr, tt.wantErr)
			}
			upd := minimalValidUpdate()
			upd.ChannelImbalanceThreshold = tt.value
			apiErr := hasValidationError(upd.Validate(), "channel_imbalance_threshold")
			if apiErr != tt.wantErr {
				t.Fatalf("Validate() rejected threshold %v = %v, want %v", tt.value, apiErr, tt.wantErr)
			}
		})
	}
}
func TestApplySettingsRoundTripsChannelImbalance(t *testing.T) {
	t.Parallel()
	cfg, configPath := newLoadedConfig(t)
	upd := minimalValidUpdate()
	upd.ChannelImbalanceThreshold = 18
	upd.ChannelImbalanceDurationMs = 20000
	upd.ChannelImbalanceRecoveryMs = 4000
	if err := cfg.ApplySettings(upd); err != nil {
		t.Fatalf("ApplySettings() error = %v", err)
	}
	reloaded := New(configPath)
	mustLoad(t, reloaded)
	snap := reloaded.Snapshot()
	assertEqual(t, "ChannelImbalanceThreshold", snap.ChannelImbalanceThreshold, 18.0)
	assertEqual(t, "ChannelImbalanceDurationMs", snap.ChannelImbalanceDurationMs, int64(20000))
	assertEqual(t, "ChannelImbalanceRecoveryMs", snap.ChannelImbalanceRecoveryMs, int64(4000))
}
func TestSettingsUpdateValidate_ClearHiddenValueConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
		set  func(*SettingsUpdate, string)
	}{
		{
			name: "graph secret",
			want: "clear_graph_client_secret: conflicts with non-empty graph_client_secret",
			set: func(upd *SettingsUpdate, value string) {
				upd.GraphClientSecret = value
				upd.ClearGraphClientSecret = true
			},
		},
		{
			name: "webhook url",
			want: "clear_webhook_url: conflicts with non-empty webhook_url",
			set: func(upd *SettingsUpdate, value string) {
				upd.WebhookURL = value
				upd.ClearWebhookURL = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, value := range []string{"new-value", "   "} {
				t.Run(fmt.Sprintf("conflict/%q", value), func(t *testing.T) {
					t.Parallel()
					upd := minimalValidUpdate()
					tt.set(upd, value)
					if !hasValidationError(upd.Validate(), tt.want) {
						t.Fatalf("Validate() = %v, want to contain %q", upd.Validate(), tt.want)
					}
				})
			}
			t.Run("empty allowed", func(t *testing.T) {
				t.Parallel()
				upd := minimalValidUpdate()
				tt.set(upd, "")
				if errs := upd.Validate(); hasValidationError(errs, tt.want) {
					t.Fatalf("Validate() unexpectedly reported conflict for empty value: %v", errs)
				}
			})
		})
	}
}
