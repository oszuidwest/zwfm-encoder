package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestCommittedSaveFailureKeepsMemoryAlignedWithDisk(t *testing.T) {
	// Keep this test serial: the injected directory-sync failure is package-wide.
	cfg, path := newLoadedConfig(t)
	failure := errors.New("directory sync failed")
	syncConfigDir = func(string) error { return failure }
	t.Cleanup(func() { syncConfigDir = syncDirPath })
	for _, mutate := range []func() error{
		func() error {
			return cfg.AddStream(&types.Stream{Host: "localhost", Port: 9000, Codec: types.CodecMP3})
		},
		func() error { return cfg.AddRecorder(newNamedRecorder(t, "Saved")) },
	} {
		if err := mutate(); !errors.Is(err, ErrSaveCommitted) || !errors.Is(err, failure) {
			t.Fatalf("post-commit failure = %v, want committed state and original error", err)
		}
		reloaded := New(path)
		mustLoad(t, reloaded)
		if !reflect.DeepEqual(reloaded.Snapshot(), cfg.Snapshot()) {
			t.Fatal("durability error rolled memory back after the disk replacement committed")
		}
	}
}

func TestStreamAndRecorderMutationsRollBackAfterSaveFailure(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"add stream", "update stream", "remove stream", "add recorder", "update recorder", "remove recorder"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			cfg, _ := newLoadedConfig(t)
			for i := range 2 {
				stream := &types.Stream{Host: "localhost", Port: 9000 + i, Codec: types.CodecMP3}
				if err := cfg.AddStream(stream); err != nil {
					t.Fatal(err)
				}
				name := []string{"One", "Two"}[i]
				if err := cfg.AddRecorder(newNamedRecorder(t, name)); err != nil {
					t.Fatal(err)
				}
			}
			before := cfg.Snapshot()
			blocked := filepath.Join(t.TempDir(), "file-not-directory")
			if err := os.WriteFile(blocked, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.filePath = filepath.Join(blocked, "config.json")
			stream, recorder := before.Streams[0], before.Recorders[0]
			var err error
			switch operation {
			case "add stream":
				err = cfg.AddStream(&types.Stream{Host: "new-host", Port: 9010, Codec: types.CodecMP3})
			case "update stream":
				stream.Host = "new-host"
				err = cfg.UpdateStream(&stream)
			case "remove stream":
				err = cfg.RemoveStream(stream.ID)
			case "add recorder":
				err = cfg.AddRecorder(newNamedRecorder(t, "Three"))
			case "update recorder":
				recorder.Name = "Changed"
				err = cfg.UpdateRecorder(&recorder)
			case "remove recorder":
				err = cfg.RemoveRecorder(recorder.ID)
			}
			if err == nil {
				t.Fatal("mutation succeeded despite blocked persistence")
			}
			if !reflect.DeepEqual(cfg.Snapshot(), before) {
				t.Fatal("failed save changed in-memory configuration")
			}
		})
	}
}

func TestFailedSettingsSavesKeepMemoryUnchanged(t *testing.T) {
	cfg, _ := newLoadedConfig(t)
	key, detectors := cfg.RecordingAPIKey(), cfg.DetectorSettings()
	settings := cfg.Snapshot()
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.filePath = filepath.Join(blocked, "config.json")
	if err := cfg.SetRecordingAPIKey("unsaved-key"); err == nil || cfg.RecordingAPIKey() != key {
		t.Fatalf("SetRecordingAPIKey = %v, key %q; want failure keeping %q", err, cfg.RecordingAPIKey(), key)
	}
	update := &SettingsUpdate{
		AudioInput: "unsaved", SilenceThreshold: -30, SilenceDurationMs: 1000, SilenceRecoveryMs: 1000, PeakHoldMs: 1000,
		ChannelImbalanceThreshold:   settings.ChannelImbalanceThreshold,
		ChannelImbalanceDurationMs:  settings.ChannelImbalanceDurationMs,
		ChannelImbalanceRecoveryMs:  settings.ChannelImbalanceRecoveryMs,
		RecordingMaxDurationMinutes: settings.RecordingMaxDurationMinutes,
	}
	if errs := update.Validate(); len(errs) > 0 {
		t.Fatalf("invalid test settings: %v", errs)
	}
	if err := cfg.ApplySettings(update); err == nil || cfg.DetectorSettings() != detectors {
		t.Fatalf("ApplySettings = %v, detectors %+v; want failure keeping %+v", err, cfg.DetectorSettings(), detectors)
	}
}
