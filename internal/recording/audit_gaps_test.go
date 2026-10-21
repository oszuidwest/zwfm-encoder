//go:build audit

package recording

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// #514: a finalize failure reported during Stop must stay in the status.
func TestAuditFinalizeFailureDuringStopStaysInStatus(t *testing.T) {
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	r, events := errorTestRecorder(t, localOnDemandRecorder(t, "stop-finalize"))
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio([]byte{1, 2, 3, 4})
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertRecorderErrorEvent(t, events, filepath.Base(file), "No space left on device") // event is logged...
	if st := r.Status(); !strings.Contains(st.Error, "No space left on device") {
		t.Fatalf("status after failed finalize on stop = %+v, want the finalize error", st) // ...but status is cleared
	}
}

// #488: an upload that a retry batch is attempting is still pending.
func TestAuditPendingUploadCountDuringRetryBatch(t *testing.T) {
	store, cfg := newIdentityStore(t)
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	r.addToRetryQueue(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true}, "down")
	if got := r.PendingUploadCount(); got != 1 {
		t.Fatalf("before batch: pending = %d, want 1", got)
	}
	var during atomic.Int64
	during.Store(-1)
	store.beforeWrite = func(string) { during.Store(int64(r.PendingUploadCount())) }
	r.processRetryQueue()
	if got := during.Load(); got != 1 {
		t.Fatalf("pending while the batch uploads = %d, want 1", got)
	}
}

// Retention cleanup deletes a local copy whose S3 upload is still pending (storage mode both).
func TestAuditRetentionKeepsFileWithPendingUpload(t *testing.T) {
	cfg := testS3Recorder()
	cfg.StorageMode, cfg.LocalPath, cfg.RetentionDays = types.StorageBoth, t.TempDir(), 1
	m, err := NewManager("", t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	old := time.Now().Add(-72 * time.Hour)
	name := r.generateFilename(old)
	file := filepath.Join(cfg.LocalPath, name)
	if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	r.addToRetryQueue(uploadRequest{localPath: file, s3Key: s3ObjectKey(&r.config, name), fileSize: 5}, "s3 down")
	if r.PendingUploadCount() != 1 {
		t.Fatal("upload not pending")
	}
	m.cleanupLocalFiles(r)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("retention deleted a file whose upload is still pending: %v", err)
	}
}

// #515: GenericRecorder.Start skips the enabled/mode checks of startHourly/startOnDemand.
func TestAuditGenericRecorderStartHonorsDisabled(t *testing.T) {
	cfg := localOnDemandRecorder(t, "disabled")
	cfg.Enabled = false
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
	t.Cleanup(func() { _ = r.Stop() })
	if err := r.Start(); !errors.Is(err, ErrRecorderDisabled) {
		t.Fatalf("Start of a disabled recorder = %v, want ErrRecorderDisabled", err)
	}
}

// currentFile is not cleared by stopEncoderAndUpload, so a stopped recorder still "owns" its last file.
func TestAuditStoppedRecorderReleasesCurrentFile(t *testing.T) {
	r, _ := errorTestRecorder(t, localOnDemandRecorder(t, "stale-current"))
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio([]byte{1, 2, 3, 4})
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	if r.IsCurrentFile(file) {
		t.Fatalf("stopped recorder still reports %s as current; retention skips it", filepath.Base(file))
	}
}
