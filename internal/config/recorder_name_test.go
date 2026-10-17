package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecorderNameCollisions(t *testing.T) {
	t.Parallel()
	for _, names := range [][2]string{
		{"Studio", "Studio!"}, {"Studio B", "Studio-B"},
		{"日本", "中文"}, {"studio", "Studio"},
	} {
		t.Run(names[0]+" and "+names[1], func(t *testing.T) {
			t.Parallel()
			for _, operation := range []string{"add", "update", "load"} {
				t.Run(operation, func(t *testing.T) {
					t.Parallel()
					cfg, path := newLoadedConfig(t)
					first := &types.Recorder{Name: names[0], Codec: types.CodecMP3,
						RecordingMode: types.RecordingOnDemand, StorageMode: types.StorageLocal,
						LocalPath: t.TempDir()}
					if err := cfg.AddRecorder(first); err != nil {
						t.Fatal(err)
					}
					candidate := *first
					candidate.ID, candidate.Name = "", names[1]
					if operation == "update" {
						candidate.Name = "Unrelated"
						if err := cfg.AddRecorder(&candidate); err != nil {
							t.Fatal(err)
						}
						candidate.Name = names[1]
					}
					before := cfg.Snapshot().Recorders
					var err error
					switch operation {
					case "add":
						err = cfg.AddRecorder(&candidate)
					case "update":
						err = cfg.UpdateRecorder(&candidate)
					case "load":
						candidate.ID = "second"
						data := cfg.ConfigData
						data.Recording.Recorders = append(data.Recording.Recorders, candidate)
						encoded, marshalErr := json.Marshal(data)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						if writeErr := os.WriteFile(path, encoded, 0o600); writeErr != nil {
							t.Fatal(writeErr)
						}
						err = New(path).Load()
					}
					if err == nil || !strings.Contains(err.Error(), "name:") || !strings.Contains(err.Error(), first.Name) {
						t.Fatalf("%s error = %v, want a name conflict identifying %q", operation, err, first.Name)
					}
					if operation != "load" {
						if !reflect.DeepEqual(cfg.Snapshot().Recorders, before) {
							t.Fatal("rejected change modified in-memory recorders")
						}
						reloaded := New(path)
						mustLoad(t, reloaded)
						if !reflect.DeepEqual(reloaded.Snapshot().Recorders, before) {
							t.Fatal("rejected change modified persisted recorders")
						}
					}
				})
			}
		})
	}
}

func TestRecorderNameAllowsOwnNameAndDistinctPrefixes(t *testing.T) {
	t.Parallel()
	cfg, path := newLoadedConfig(t)
	for _, name := range []string{"Studio", "Studio-B"} {
		recorder := &types.Recorder{Name: name, Codec: types.CodecMP3,
			RecordingMode: types.RecordingOnDemand, StorageMode: types.StorageLocal,
			LocalPath: t.TempDir()}
		if err := cfg.AddRecorder(recorder); err != nil {
			t.Fatal(err)
		}
		if err := cfg.UpdateRecorder(recorder); err != nil {
			t.Fatalf("updating own unchanged name: %v", err)
		}
	}
	mustLoad(t, New(path))
}
