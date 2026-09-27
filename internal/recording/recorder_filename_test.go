package recording

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestNextRecordingFilename(t *testing.T) {
	t.Parallel()

	startMinute := time.Date(2026, time.September, 28, 14, 37, 42, 0, time.Local)
	hourBoundary := time.Date(2026, time.September, 28, 15, 0, 0, 0, time.Local)
	tests := []struct {
		name      string
		startTime time.Time
		existing  []string
		used      []string
		want      string
	}{
		{
			name:      "mid-hour start uses actual minute",
			startTime: startMinute,
			want:      "Studio-2026-09-28-14-37.mp3",
		},
		{
			name:      "rotation at hour boundary keeps HH-00",
			startTime: hourBoundary,
			want:      "Studio-2026-09-28-15-00.mp3",
		},
		{
			name:      "collision with existing file",
			startTime: startMinute,
			existing:  []string{"Studio-2026-09-28-14-37.mp3"},
			want:      "Studio-2026-09-28-14-37-2.mp3",
		},
		{
			name:      "collision with previously used name",
			startTime: startMinute,
			used:      []string{"Studio-2026-09-28-14-37.mp3"},
			want:      "Studio-2026-09-28-14-37-2.mp3",
		},
		{
			name:      "multiple collisions",
			startTime: startMinute,
			existing: []string{
				"Studio-2026-09-28-14-37.mp3",
				"Studio-2026-09-28-14-37-2.mp3",
			},
			used: []string{
				"Studio-2026-09-28-14-37-3.mp3",
			},
			want: "Studio-2026-09-28-14-37-4.mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			existing := make(map[string]struct{}, len(tt.existing))
			for _, filename := range tt.existing {
				existing[filename] = struct{}{}
			}
			used := make(map[string]struct{}, len(tt.used))
			for _, filename := range tt.used {
				used[filename] = struct{}{}
			}
			cfg := types.Recorder{
				Name:  "Studio",
				Codec: types.CodecMP3,
			}

			got := nextRecordingFilename(&cfg, tt.startTime, func(filename string) bool {
				_, ok := existing[filename]
				return ok
			}, used)
			if got != tt.want {
				t.Fatalf("nextRecordingFilename() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilenameTimeParsesSuffixedRecordingName(t *testing.T) {
	t.Parallel()

	got, ok := util.FilenameTime("Studio-2026-09-28-14-37-2.mp3")
	if !ok {
		t.Fatal("FilenameTime() did not parse a collision-suffixed recording name")
	}
	want := time.Date(2026, time.September, 28, 14, 37, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("FilenameTime() = %v, want %v", got, want)
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
	dir := t.TempDir()
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder: &types.Recorder{
			ID:            "r1",
			Name:          "Studio",
			Codec:         types.CodecPCM,
			RecordingMode: types.RecordingOnDemand,
			StorageMode:   types.StorageLocal,
			LocalPath:     dir,
		},
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
