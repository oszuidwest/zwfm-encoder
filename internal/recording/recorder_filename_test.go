package recording

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
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

func TestGenerateFilenameUsesStartMinute(t *testing.T) {
	t.Parallel()

	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder: &types.Recorder{Name: "Studio", Codec: types.CodecMP3},
	})
	tests := map[time.Time]string{
		time.Date(2026, time.September, 28, 14, 37, 42, 0, time.Local): "Studio-2026-09-28-14-37.mp3",
		time.Date(2026, time.September, 28, 15, 0, 0, 0, time.Local):   "Studio-2026-09-28-15-00.mp3",
	}
	for startTime, want := range tests {
		if got := recorder.generateFilename(startTime); got != want {
			t.Errorf("generateFilename(%v) = %q, want %q", startTime, got, want)
		}
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

// TestRestartDoesNotReuseDeletedRecordingName covers S3-only mode, where the
// spool file is deleted after upload: the name must not come back while the
// recorder runs, or the next upload would overwrite the earlier object.
func TestRestartDoesNotReuseDeletedRecordingName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake FFmpeg is a POSIX shell script")
	}

	fakeFFmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nfor arg; do out=$arg; done\ncat > \"$out\"\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o700); err != nil { //nolint:gosec // Test executable.
		t.Fatal(err)
	}
	cfg := localOnDemandRecorder(t, "Studio")
	dir := cfg.LocalPath
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder:   cfg,
		FFmpegPath: fakeFFmpeg,
		SpoolDir:   t.TempDir(),
	})

	seen := map[string]bool{}
	for range 3 {
		if err := recorder.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for recorder.Status().State != types.ProcessRunning {
			if time.Now().After(deadline) {
				t.Fatalf("recorder state = %+v, want running", recorder.Status())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := recorder.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("recording dir = %v, %v; want exactly one file", entries, err)
		}
		name := entries[0].Name()
		if seen[name] {
			t.Fatalf("recording name %q reused after the earlier file was removed", name)
		}
		seen[name] = true
		// Simulate the spool cleanup that follows a successful S3 upload.
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}
