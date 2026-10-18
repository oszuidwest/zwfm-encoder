package recording

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestNextRecordingFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		taken []string
		want  string
	}{
		{
			name: "free name is kept",
			want: "Studio-2026-09-28-14-37.mp3",
		},
		{
			name:  "taken name gets -2",
			taken: []string{"Studio-2026-09-28-14-37.mp3"},
			want:  "Studio-2026-09-28-14-37-2.mp3",
		},
		{
			name: "multiple collisions",
			taken: []string{
				"Studio-2026-09-28-14-37.mp3",
				"Studio-2026-09-28-14-37-2.mp3",
				"Studio-2026-09-28-14-37-3.mp3",
			},
			want: "Studio-2026-09-28-14-37-4.mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := nextRecordingFilename("Studio-2026-09-28-14-37.mp3", func(name string) bool {
				return slices.Contains(tt.taken, name)
			})
			if got != tt.want {
				t.Fatalf("nextRecordingFilename() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRememberFilenameForgetsNamesAfterWindow(t *testing.T) {
	t.Parallel()

	recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: &types.Recorder{}})
	recorder.usedFilenames["old.ts"] = time.Now().Add(-usedFilenameWindow - time.Minute)
	recorder.usedFilenames["recent.ts"] = time.Now().Add(-usedFilenameWindow + time.Minute)

	recorder.rememberFilenameLocked("new.ts")

	got := slices.Sorted(maps.Keys(recorder.usedFilenames))
	if want := []string{"new.ts", "recent.ts"}; !slices.Equal(got, want) {
		t.Fatalf("remembered names = %v, want %v", got, want)
	}
}

// recordOnce starts and stops the recorder and returns the files it created in dir.
func recordOnce(t *testing.T, recorder *GenericRecorder, dir string) []string {
	t.Helper()

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForRecorderState(t, recorder, types.ProcessRunning)
	recorder.WriteAudio([]byte{1, 2, 3, 4})
	if err := recorder.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, entry := range after {
		if !slices.ContainsFunc(before, func(e os.DirEntry) bool { return e.Name() == entry.Name() }) {
			created = append(created, entry.Name())
		}
	}
	return created
}

// TestHourlyStartKeepsEarlierRecordingOfSameMinute covers an app restart: the
// name history is empty, so only the exclusive reservation protects it.
func TestHourlyStartKeepsEarlierRecordingOfSameMinute(t *testing.T) {
	t.Parallel()

	cfg := localOnDemandRecorder(t, "Studio")
	cfg.RecordingMode = types.RecordingHourly
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder:   cfg,
		FFmpegPath: os.Args[0],
		SpoolDir:   t.TempDir(),
	})

	// Cover both minutes the start can fall in.
	start := time.Now()
	var earlier []string
	for _, minute := range []time.Time{start, start.Add(time.Minute)} {
		name := recorder.generateFilename(minute)
		if err := os.WriteFile(filepath.Join(cfg.LocalPath, name), []byte("earlier"), 0o600); err != nil {
			t.Fatal(err)
		}
		earlier = append(earlier, name)
	}

	// A start just before the hour may also rotate, so allow a second file.
	created := recordOnce(t, recorder, cfg.LocalPath)
	if len(created) == 0 {
		t.Fatal("no new recording created")
	}
	for _, name := range earlier {
		data, err := os.ReadFile(filepath.Join(cfg.LocalPath, name)) //nolint:gosec // Test temp dir.
		if err != nil || string(data) != "earlier" {
			t.Errorf("earlier recording %s = %q, %v; want it untouched", name, data, err)
		}
	}
	// Named after the start minute, not the hour start (unless that is the same).
	for _, name := range created {
		if got, ok := util.FilenameTime(name); !ok || got.Before(start.Truncate(time.Minute)) {
			t.Errorf("new recording %s starts at %v, want at or after %v", name, got, start.Truncate(time.Minute))
		}
	}
}

// TestRestartDoesNotReuseDeletedRecordingName simulates S3-only mode, where
// the spool file is deleted after upload: the name must not come back within
// usedFilenameWindow, or the next upload would reuse the earlier object's key.
func TestRestartDoesNotReuseDeletedRecordingName(t *testing.T) {
	t.Parallel()

	cfg := localOnDemandRecorder(t, "Studio")
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder:   cfg,
		FFmpegPath: os.Args[0],
		SpoolDir:   t.TempDir(),
	})

	// Three runs: at most one minute change fits in them, so two share a minute.
	seen := map[string]bool{}
	for range 3 {
		created := recordOnce(t, recorder, cfg.LocalPath)
		if len(created) != 1 {
			t.Fatalf("created files = %v, want one new recording", created)
		}
		name := created[0]
		if seen[name] {
			t.Fatalf("recording name %q reused after the earlier file was removed", name)
		}
		seen[name] = true
		// Simulate the spool cleanup that follows a successful S3 upload.
		if err := os.Remove(filepath.Join(cfg.LocalPath, name)); err != nil {
			t.Fatal(err)
		}
	}
}
