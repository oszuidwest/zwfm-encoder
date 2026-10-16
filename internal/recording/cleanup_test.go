package recording

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecordingFileTime(t *testing.T) {
	t.Parallel()

	wantTime := time.Date(2026, time.September, 28, 14, 37, 0, 0, time.Local)
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
			name:     "extension of a removed codec",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37.ogg",
			wantTime: wantTime,
			wantOK:   true,
		},
		{
			name:     "file of another tool",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37.json",
		},
		{
			name:     "double extension",
			safeName: "Studio",
			filename: "Studio-2026-09-28-14-37.mp3.bak",
		},
		{
			name:     "recorder name containing a timestamp",
			safeName: "Studio-2024-01-01-00-00",
			filename: "Studio-2024-01-01-00-00-2026-09-28-14-37.ts",
			wantTime: wantTime,
			wantOK:   true,
		},
		{
			name:     "file of a recorder whose name extends ours with a timestamp",
			safeName: "Studio",
			filename: "Studio-2024-01-01-00-00-2026-09-28-14-37.ts",
		},
		{
			name:     "unrelated file",
			safeName: "Studio",
			filename: "notes.txt",
		},
		{
			name:     "underscore timestamp layout",
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
				t.Fatalf("recordingFileTime(%q, %q) ok = %t, want %t", tt.safeName, tt.filename, gotOK, tt.wantOK)
			}
			if tt.wantOK && !gotTime.Equal(tt.wantTime) {
				t.Errorf("recordingFileTime(%q, %q) = %v, want %v", tt.safeName, tt.filename, gotTime, tt.wantTime)
			}
		})
	}
}

// TestRecordingFileTimeMatchesGeneratedNames ties the matcher to the writer, so
// a naming change cannot silently stop retention.
func TestRecordingFileTimeMatchesGeneratedNames(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.September, 28, 14, 37, 0, 0, time.Local)
	for _, codec := range []types.Codec{types.CodecMP3, types.CodecOpus, types.CodecPCM} {
		recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: &types.Recorder{Name: "Studio B!", Codec: codec}})
		name := recorder.generateFilename(start)
		if got, ok := recordingFileTime(sanitizeFilename("Studio B!"), name); !ok || !got.Equal(start) {
			t.Errorf("recordingFileTime(%q) = %v, %t; want %v, true", name, got, ok, start)
		}
	}
}

func TestCleanupLocalFilesDeletesOnlyExpiredOwnedFiles(t *testing.T) {
	t.Parallel()

	cfg := localOnDemandRecorder(t, "Studio")
	cfg.RetentionDays = 1
	directory := cfg.LocalPath
	recentFile := "Studio-" + time.Now().Format(recordingTimestampLayout) + ".mp3"
	files := []struct {
		name       string
		wantExists bool
	}{
		{name: "Studio-2000-01-01-00-00.mp3"},
		{name: "Studio-2000-01-01-00-01-2.mp3"},
		{name: "Studio-2000-01-01-00-03.ogg"},
		{name: "Studio-2000-01-01-00-02.mp3", wantExists: true},
		{name: "Studio-2000-01-01-00-04.json", wantExists: true},
		{name: "Studio-2000-01-01-00-05.mp3", wantExists: true}, // Old name, written just now.
		{name: recentFile, wantExists: true},
		{name: "Studio-B-2000-01-01-00-00.mp3", wantExists: true},
	}
	old := time.Now().AddDate(0, 0, -2)
	for _, file := range files {
		writeCleanupFile(t, directory, file.name)
		if file.name != "Studio-2000-01-01-00-05.mp3" {
			if err := os.Chtimes(filepath.Join(directory, file.name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	currentFile := filepath.Join(directory, "Studio-2000-01-01-00-02.mp3")
	recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
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

func TestCleanupS3FilesDeletesOnlyObjectsExpiredByNameAndUploadTime(t *testing.T) {
	t.Parallel()

	old := time.Now().AddDate(0, 0, -2).UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)
	var (
		mu      sync.Mutex
		deleted []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>false</IsTruncated>
<Contents><Key>recordings/Test/Test-2000-01-01-00-00.mp3</Key><LastModified>%s</LastModified></Contents>
<Contents><Key>recordings/Test/Test-2000-01-01-01-00.mp3</Key><LastModified>%s</LastModified></Contents>
</ListBucketResult>`, old, recent)
		case http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/bucket/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	cfg.RetentionDays = 1
	(&Manager{}).cleanupS3Files(NewGenericRecorder(GenericRecorderConfig{Recorder: cfg}))

	mu.Lock()
	defer mu.Unlock()
	// The second object has an old name but was uploaded just now.
	if want := []string{"recordings/Test/Test-2000-01-01-00-00.mp3"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
}
