package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func newNamedRecorder(t *testing.T, name string) *types.Recorder {
	t.Helper()
	return &types.Recorder{Name: name, Codec: types.CodecMP3, RecordingMode: types.RecordingOnDemand,
		StorageMode: types.StorageLocal, LocalPath: t.TempDir()}
}

func TestRecorderNameCollisions(t *testing.T) {
	t.Parallel()
	for _, names := range [][2]string{{"studio", "Studio"}, {"Studio B", "studio b"}} {
		for _, operation := range []string{"add", "update"} {
			t.Run(operation+" "+names[0]+" and "+names[1], func(t *testing.T) {
				t.Parallel()
				update := operation == "update"
				cfg, path := newLoadedConfig(t)
				if err := cfg.AddRecorder(newNamedRecorder(t, names[0])); err != nil {
					t.Fatal(err)
				}
				candidate := newNamedRecorder(t, names[1])
				if update {
					candidate.Name = "Unrelated"
					if err := cfg.AddRecorder(candidate); err != nil {
						t.Fatal(err)
					}
					candidate.Name = names[1]
				}
				before := cfg.Snapshot().Recorders

				var err error
				if update {
					err = cfg.UpdateRecorder(candidate)
				} else {
					err = cfg.AddRecorder(candidate)
				}
				if !errors.Is(err, ErrInvalidRecorderConfig) || !strings.Contains(err.Error(), names[0]) {
					t.Fatalf("%s error = %v, want a duplicate-name error naming %q", operation, err, names[0])
				}
				if !reflect.DeepEqual(cfg.Snapshot().Recorders, before) {
					t.Fatal("rejected change modified in-memory recorders")
				}
				reloaded := New(path)
				mustLoad(t, reloaded)
				if !reflect.DeepEqual(reloaded.Snapshot().Recorders, before) {
					t.Fatal("rejected change modified persisted recorders")
				}
			})
		}
	}
}

func TestRecorderNameAllowsOwnNameAndDistinctNames(t *testing.T) {
	t.Parallel()
	cfg, path := newLoadedConfig(t)
	for _, name := range []string{"Studio", "Studio ", "Studio!", "Studio B", "Studio-B", "日本", "中文"} {
		recorder := newNamedRecorder(t, name)
		if err := cfg.AddRecorder(recorder); err != nil {
			t.Fatal(err)
		}
		if err := cfg.UpdateRecorder(recorder); err != nil {
			t.Fatalf("updating own unchanged name: %v", err)
		}
	}
	mustLoad(t, New(path))
}
