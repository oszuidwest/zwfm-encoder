package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRecorderFilePrefixPersistsThroughRenameAndReload(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(fullyPopulatedConfigData(t))
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, string(data))
	cfg := New(path)
	mustLoad(t, cfg)
	saved, err := os.ReadFile(path) //nolint:gosec // Test temp config.
	if err != nil || !strings.Contains(string(saved), `"file_prefix": "Archive"`) {
		t.Fatalf("assigned file prefix not persisted on load: %v\n%s", err, saved)
	}
	updated := *cfg.Recorder("recorder-1")
	updated.Name, updated.FilePrefix = "After", "Other"
	if err := cfg.UpdateRecorder(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.FilePrefix != "Archive" {
		t.Fatalf("caller file prefix = %q, want Archive", updated.FilePrefix)
	}
	cfg = New(path)
	mustLoad(t, cfg)
	if got := cfg.Recorder("recorder-1"); got.FilePrefix != "Archive" || got.Name != "After" {
		t.Fatalf("rename or reload changed the file prefix: %+v", got)
	}
	for _, names := range [][2]string{{"Studio B", "Studio-B"}, {"Studio-B", "Studio-B-2"}, {"studio b!", "studio-b-3"}, {strings.Repeat("a", 99), strings.Repeat("a", 64)}} {
		recorder := newNamedRecorder(t, names[0])
		recorder.FilePrefix = "Archive" // A client-sent prefix is replaced.
		if err := cfg.AddRecorder(recorder); err != nil {
			t.Fatal(err)
		}
		if got := cfg.Recorder(recorder.ID).FilePrefix; got != names[1] || recorder.FilePrefix != got {
			t.Fatalf("file prefix of %q = %q (caller sees %q), want %q", names[0], got, recorder.FilePrefix, names[1])
		}
	}
}

func TestRecorderFilePrefixesMustDifferIgnoringCase(t *testing.T) {
	t.Parallel()
	data := fullyPopulatedConfigData(t)
	second := data.Recording.Recorders[0]
	data.Recording.Recorders[0].FilePrefix = "Archive"
	second.ID, second.Name, second.FilePrefix = "another-id", "Another name", "archive"
	data.Recording.Recorders = append(data.Recording.Recorders, second)
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(writeConfig(t, string(encoded))).Load(); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("Load() error = %v, want a duplicate file_prefix error", err)
	}
}
