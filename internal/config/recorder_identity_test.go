package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyRecorderIdentityPersistsThroughReplacementAndReload(t *testing.T) {
	t.Parallel()
	legacy := fullyPopulatedConfigData(t)
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, string(data))
	cfg := New(path)
	mustLoad(t, cfg)
	id := legacy.Recording.Recorders[0].ID
	prefix := cfg.Recorder(id).RecordingFilePrefix()
	for _, name := range []string{"After", "Again"} {
		updated := *cfg.Recorder(id)
		updated.Name = name
		// A full API replacement cannot erase or forge internal ownership.
		updated.IDBasedFiles, updated.LegacyFilePrefix = false, "OtherRecorder"
		if err := cfg.UpdateRecorder(&updated); err != nil {
			t.Fatal(err)
		}
		cfg = New(path)
		mustLoad(t, cfg)
		saved := cfg.Recorder(id)
		if !saved.IDBasedFiles || saved.LegacyFilePrefix != "Archive" || saved.RecordingFilePrefix() != prefix || saved.Name != name {
			t.Fatalf("rename or reload changed ownership: %+v", saved)
		}
	}
	newRecorder := newNamedRecorder(t, "Archive")
	if err := cfg.AddRecorder(newRecorder); err != nil {
		t.Fatal(err)
	}
	if !newRecorder.IDBasedFiles || newRecorder.LegacyFilePrefix != "" || newRecorder.RecordingFilePrefix() == prefix {
		t.Fatal("a new ID adopted the earlier recorder's legacy prefix")
	}
	if len(strings.TrimPrefix(newRecorder.ID, "recorder-")) != 32 {
		t.Fatal("persistent recorder ownership needs a 128-bit random ID")
	}
	mustLoad(t, New(path))
}

func TestLegacyRecorderClaimsCannotOverlapOrUseIDNamespace(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"duplicate claim", "ID namespace"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			data := fullyPopulatedConfigData(t)
			first := &data.Recording.Recorders[0]
			first.InitializeFileIdentity()
			if invalid == "duplicate claim" {
				second := *first
				second.ID, second.Name = "another-id", "Another name"
				data.Recording.Recorders = append(data.Recording.Recorders, second)
			} else {
				first.LegacyFilePrefix = first.RecordingFilePrefix()
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			err = New(writeConfig(t, string(encoded))).Load()
			if err == nil || !strings.Contains(err.Error(), "legacy_file_prefix") {
				t.Fatalf("invalid ownership accepted: %v", err)
			}
		})
	}
}
