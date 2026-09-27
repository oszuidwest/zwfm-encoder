package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecordingFileTime(t *testing.T) {
	t.Parallel()

	wantTime := time.Date(
		2026,
		time.September,
		28,
		14,
		37,
		0,
		0,
		time.Local,
	)
	tests := []struct {
		name     string
		safeName string
		filename string
		wantTime time.Time
		wantOK   bool
	}{
		{
			name:     "own file",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37.mp3",
			wantTime: wantTime,
			wantOK:   true,
		},
		{
			name:     "other recorder with shared prefix",
			safeName: "Studio",
			filename: "Studio-B-2026-09-28-14-37.mp3",
		},
		{
			name:     "numeric collision suffix",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37-2.mp3",
			wantTime: wantTime,
			wantOK:   true,
		},
		{
			name:     "wrong extension",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37.wav",
		},
		{
			name:     "recorder name containing a timestamp",
			safeName: "Studio-2024-01-01-00-00",
			filename: "Studio-2024-01-01-00-00-2026-09-28-14-37.ts",
			wantTime: wantTime,
			wantOK:   true,
		},
		{
			name:     "unrelated file",
			safeName: "Studio",
			filename: "notes.txt",
		},
		{
			name:     "silence dump timestamp",
			safeName: "Studio",
			filename: "Studio-2026-09-28_14-37-00-2.mp3",
		},
		{
			name:     "non-numeric collision suffix",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37-copy.mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotTime, gotOK := recordingFileTime(tt.safeName, tt.filename)
			if gotOK != tt.wantOK {
				t.Fatalf(
					"recordingFileTime(%q, %q) ok = %t, want %t",
					tt.safeName,
					tt.filename,
					gotOK,
					tt.wantOK,
				)
			}
			if tt.wantOK && !gotTime.Equal(tt.wantTime) {
				t.Errorf(
					"recordingFileTime(%q, %q) = %v, want %v",
					tt.safeName,
					tt.filename,
					gotTime,
					tt.wantTime,
				)
			}
		})
	}
}

func TestCleanupLocalFilesDeletesOnlyExpiredOwnedFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	recentFile := "Studio-" + time.Now().Format(recordingTimestampLayout) + ".mp3"
	files := []struct {
		name       string
		wantExists bool
	}{
		{name: "Studio-2000-01-01-00-00.mp3"},
		{name: "Studio-2000-01-01-00-01-2.mp3"},
		{name: "Studio-2000-01-01-00-03.ts"},
		{name: "Studio-2000-01-01-00-02.mp3", wantExists: true},
		{name: recentFile, wantExists: true},
		{name: "Studio-B-2000-01-01-00-00.mp3", wantExists: true},
	}
	for _, file := range files {
		writeCleanupFile(t, directory, file.name)
	}

	currentFile := filepath.Join(directory, "Studio-2000-01-01-00-02.mp3")
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder: &types.Recorder{
			ID:            "studio",
			Name:          "Studio",
			Codec:         types.CodecMP3,
			StorageMode:   types.StorageLocal,
			LocalPath:     directory,
			RetentionDays: 1,
		},
	})
	recorder.currentFile = currentFile

	manager := &Manager{}
	manager.cleanupLocalFiles(recorder)

	for _, file := range files {
		path := filepath.Join(directory, file.name)
		_, err := os.Stat(path)
		switch {
		case file.wantExists && err != nil:
			t.Errorf("expected %q to survive cleanup: %v", file.name, err)
		case !file.wantExists && !os.IsNotExist(err):
			t.Errorf("expected %q to be deleted, stat error = %v", file.name, err)
		}
	}
}

func writeCleanupFile(t *testing.T, directory, name string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(directory, name), []byte("audio"), 0o600); err != nil {
		t.Fatalf("write cleanup file %q: %v", name, err)
	}
}
