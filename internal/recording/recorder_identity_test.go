package recording

import (
	"bytes"
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

func TestRecorderFileIdentitySurvivesRename(t *testing.T) {
	t.Parallel()
	cfg := localOnDemandRecorder(t, "stable-id")
	cfg.Name = "Before"
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
	at := time.Date(2026, 10, 18, 14, 0, 0, 0, time.Local)
	before := r.generateFilename(at)
	updated := *cfg
	updated.Name = "After"
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	if after := r.generateFilename(at); after != before {
		t.Fatalf("rename changed recording identity: %q -> %q", before, after)
	}
}

func TestRecorderRenameKeepsLegacyAndIDS3Retention(t *testing.T) {
	t.Parallel()
	// An IAM policy still scoped to the old prefix may deny listing the ID namespace.
	for _, deniedID := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied ID listing=%t", deniedID), func(t *testing.T) {
			t.Parallel()
			cfg := testS3Recorder()
			cfg.Name, cfg.RetentionDays = "Before", 1
			prefix := cfg.RecordingFilePrefix()
			oldKey := "recordings/Before/Before-2000-01-01-00-00.mp3"
			idKey := "recordings/" + prefix + "/" + prefix + "-2000-01-01-00-00.mp3"
			var mu sync.Mutex
			var listed, deleted []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if req.Method == http.MethodDelete {
					deleted = append(deleted, strings.TrimPrefix(req.URL.Path, "/bucket/"))
					w.WriteHeader(http.StatusNoContent)
					return
				}
				listed = append(listed, req.URL.Query().Get("prefix"))
				if deniedID && req.URL.Query().Get("prefix") == "recordings/"+prefix+"/" {
					writeS3Error(w, http.StatusForbidden, "AccessDenied")
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				// Include unrelated and nested objects too: only our exact namespaces count.
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>%s</Key></Contents><Contents><Key>%s</Key></Contents>
<Contents><Key>recordings/Before-B/Before-B-2000-01-01-00-00.mp3</Key></Contents>
<Contents><Key>recordings/Before/nested/Before-2000-01-01-00-00.mp3</Key></Contents>
<Contents><Key>recordings/@deadbeef/file-2000-01-01-00-00.mp3</Key></Contents>
</ListBucketResult>`, oldKey, idKey)
			}))
			t.Cleanup(server.Close)
			cfg.S3Endpoint = server.URL
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
			updated := *cfg
			updated.Name = "After"
			if err := r.UpdateConfig(&updated); err != nil {
				t.Fatal(err)
			}
			(&Manager{}).cleanupS3Files(r)
			mu.Lock()
			defer mu.Unlock()
			slices.Sort(listed)
			slices.Sort(deleted)
			wantListed, wantDeleted := []string{"recordings/" + prefix + "/", "recordings/Before/"}, []string{idKey, oldKey}
			if deniedID {
				wantDeleted = []string{oldKey} // The legacy namespace is still cleaned.
			}
			slices.Sort(wantListed)
			slices.Sort(wantDeleted)
			if !slices.Equal(listed, wantListed) || !slices.Equal(deleted, wantDeleted) {
				t.Fatalf("rename lost retention ownership: listed=%v, deleted=%v", listed, deleted)
			}
		})
	}
}

func TestRecorderRenamePreservesPendingLegacyUploadIdentity(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	cfg.Name = "Before"
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, cfg.ID, "Before-2026-10-18-14-00.mp3", "audio")
	key := "recordings/Before/Before-2026-10-18-14-00.mp3"
	pending := pendingUpload{request: uploadRequest{localPath: file, s3Key: key, fileSize: 5, deleteAfterUpload: true}, uploadID: "original-upload-id"}
	if err := r.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	updated := r.Config()
	updated.Name = "After"
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	restarted := NewGenericRecorder(GenericRecorderConfig{Recorder: &updated, SpoolDir: r.spoolDir})
	if err := restarted.reconcilePendingUploads(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.retryQueue) != 1 {
		t.Fatalf("recovered uploads = %d, want 1", len(restarted.retryQueue))
	}
	got := restarted.retryQueue[0]
	if got.uploadID != pending.uploadID || got.baseKey != pending.baseKey || got.request.s3Key != key || got.request.localPath != file {
		t.Fatalf("rename changed persisted upload identity: %+v", got)
	}
	restarted.processRetryQueue()
	store.mu.Lock()
	defer store.mu.Unlock()
	object, ok := store.objects[key]
	if !ok || object.id != pending.uploadID || string(object.body) != "audio" || len(store.objects) != 1 {
		t.Fatalf("legacy retry changed identity or duplicated audio: %+v", store.objects)
	}
}

func TestLiveRenameKeepsStableNewObjectKeyAndAudio(t *testing.T) {
	store, cfg := newIdentityStore(t)
	cfg.Name, cfg.RecordingMode = "Before", types.RecordingOnDemand
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
	file := startCaptureTestRecorder(t, r)
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 1024)
	r.WriteAudio(pcm)
	updated := r.Config()
	updated.Name = strings.Repeat("Very long display name", 20)
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	r.WriteAudio(pcm)
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	waitForRetryPass(t, r) // Stop leaves the final file to the retry queue.
	key := "recordings/" + cfg.RecordingFilePrefix() + "/" + filepath.Base(file)
	store.mu.Lock()
	defer store.mu.Unlock()
	object, ok := store.objects[key]
	if !ok || !bytes.Equal(object.body, append(pcm, pcm...)) || len(store.objects) != 1 {
		t.Fatalf("live rename lost or moved audio: %+v", store.objects)
	}
}

func TestNewRecorderDoesNotAdoptLegacyFilesWithReusedName(t *testing.T) {
	t.Parallel()
	cfg := localOnDemandRecorder(t, "new-id")
	cfg.Name, cfg.RetentionDays, cfg.IDBasedFiles = "Reused", 1, true
	file := filepath.Join(cfg.LocalPath, "Reused-2000-01-01-00-00.ts")
	if err := os.WriteFile(file, []byte("foreign audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
	own := filepath.Join(cfg.LocalPath, cfg.RecordingFilePrefix()+"-2000-01-01-00-00.ts")
	if err := os.WriteFile(own, []byte("own audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(own, old, old); err != nil {
		t.Fatal(err)
	}
	(&Manager{}).cleanupLocalFiles(r)
	assertRecordedCapture(t, file, []byte("foreign audio"))
	assertNoRecording(t, own) // Retention itself still ran.
}

func TestRecorderRenameKeepsLegacyLocalRetention(t *testing.T) {
	t.Parallel()
	cfg := localOnDemandRecorder(t, "stable-id")
	cfg.Name, cfg.RetentionDays = "Before", 1
	path := filepath.Join(cfg.LocalPath, "Before-2000-01-01-00-00.ts")
	if err := os.WriteFile(path, []byte("old audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
	idFile := r.generateFilename(time.Date(2000, 1, 1, 0, 0, 0, 0, time.Local))
	writeCleanupFile(t, cfg.LocalPath, idFile)
	if err := os.Chtimes(filepath.Join(cfg.LocalPath, idFile), old, old); err != nil {
		t.Fatal(err)
	}
	updated := *cfg
	updated.Name = "After"
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	(&Manager{}).cleanupLocalFiles(r)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rename stranded expired legacy recording: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.LocalPath, idFile)); !os.IsNotExist(err) {
		t.Fatalf("rename stranded expired ID recording: %v", err)
	}
}

func TestRecorderRuntimeRejectsIdentityChange(t *testing.T) {
	t.Parallel()
	cfg := localOnDemandRecorder(t, "immutable")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg})
	before := r.Config()
	updated := before
	updated.ID = "another"
	if err := r.UpdateConfig(&updated); err == nil || r.Config() != before {
		t.Fatal("runtime update changed the recorder's identity")
	}
}

func TestLongRecorderIDAndDisplayNameStillProduceWritableOutput(t *testing.T) {
	cfg := localOnDemandRecorder(t, strings.Repeat("r", 250))
	cfg.Name = strings.Repeat("Very long display name", 20)
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio([]byte{1, 2, 3, 4})
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertRecordedCapture(t, file, []byte{1, 2, 3, 4})
}

func TestSpoolRecoveryDoesNotAdoptForeignFilesInAliasedDirectory(t *testing.T) {
	t.Parallel()
	cfg := testS3Recorder()
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	at := time.Date(2026, 10, 18, 14, 0, 0, 0, time.Local)
	ownedName := r.generateFilename(at)
	foreign := *cfg
	foreign.ID = strings.ToUpper(cfg.ID)
	foreignName := foreign.RecordingFilePrefix() + "-2026-10-18-14-00.mp3"
	for _, name := range []string{ownedName, "Test-2026-10-18-14-00.mp3", foreignName, "Unrelated-2026-10-18-14-00.mp3"} {
		writeSpoolFile(t, r.spoolDir, cfg.ID, name, "audio")
	}
	if err := r.reconcilePendingUploads(); err != nil {
		t.Fatal(err)
	}
	if len(r.retryQueue) != 2 {
		t.Fatalf("recovery adopted an unowned file: %+v", r.retryQueue)
	}
	for _, pending := range r.retryQueue {
		if name := filepath.Base(pending.request.localPath); name != ownedName && name != "Test-2026-10-18-14-00.mp3" {
			t.Fatalf("foreign output queued: %s", name)
		}
	}
}
