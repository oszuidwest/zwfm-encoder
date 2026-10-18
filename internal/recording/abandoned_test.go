package recording

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
)

func TestAbandonedSpoolUploadsAfterConfigFixAndRestart(t *testing.T) {
	t.Parallel()
	spoolDir := t.TempDir()
	var fixed atomic.Bool
	var attempts, uploads atomic.Int32
	server := httptest.NewServer(abandonedUploadHandler(t, &fixed, &attempts, &uploads))
	defer server.Close()
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	eventPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	var alerts []UploadAbandonedEvent
	r := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg, SpoolDir: spoolDir, EventLogger: logger,
		OnUploadAbandoned: func(event UploadAbandonedEvent) { alerts = append(alerts, event) },
	})
	file := writeSpoolFile(t, spoolDir, cfg.ID, "expired.mp3", "audio")
	req := uploadRequest{localPath: file, s3Key: "recordings/Original/expired-2.mp3", fileSize: 5, deleteAfterUpload: true}
	r.uploadFile(req)
	if len(r.retryQueue) != 1 {
		t.Fatal("failed upload was not persisted for retry")
	}
	r.retryQueue[0].firstAttempt = time.Now().Add(-MaxUploadRetryAge - time.Hour)
	r.retryQueue[0].retryCount = 3
	if err := r.savePendingUpload(&r.retryQueue[0]); err != nil {
		t.Fatal(err)
	}
	metadataPath := r.retryQueue[0].metadataPath
	r.processRetryQueue()
	r.processRetryQueue() // Abandoned uploads do not keep retrying in this process.
	if r.PendingUploadCount() != 0 || attempts.Load() != 1 || len(alerts) != 1 {
		t.Fatalf("after abandonment: pending %d, attempts %d, alerts %+v", r.PendingUploadCount(), attempts.Load(), alerts)
	}
	assertAbandonmentAlert(t, eventPath, &alerts[0], req)
	if data, err := os.ReadFile(file); err != nil || string(data) != "audio" { //nolint:gosec // Test temp recording.
		t.Fatalf("abandoned recording = %q, %v; want preserved audio", data, err)
	}
	fixed.Store(true)
	cfg.Name, cfg.S3AccessKeyID = "Renamed", "fixed-access"
	restarted, err := NewManager("", spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := restarted.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if got := restarted.PendingUploadCount(); got != 1 {
		t.Fatalf("startup scan restored %d uploads, want 1", got)
	}
	restored := restarted.recorders[cfg.ID].retryQueue[0]
	if restored.retryCount != 0 || restored.firstAttempt.Before(before) || restored.firstAttempt.After(after) {
		t.Fatalf("startup did not reset the abandoned retry window: %+v", restored)
	}
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Stop() }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, fileErr := os.Stat(file)
		_, metadataErr := os.Stat(metadataPath)
		if os.IsNotExist(fileErr) && os.IsNotExist(metadataErr) && uploads.Load() == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recovery did not finish: attempts %d, uploads %d, pending %d", attempts.Load(), uploads.Load(), restarted.PendingUploadCount())
}

func abandonedUploadHandler(t *testing.T, fixed *atomic.Bool, attempts, uploads *atomic.Int32) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			attempts.Add(1)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		if !fixed.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		if req.Method != http.MethodPut || req.URL.Path != "/bucket/recordings/Original/expired-2.mp3" ||
			req.Header.Get("If-None-Match") != "*" || string(body) != "audio" ||
			!strings.Contains(req.Header.Get("Authorization"), "fixed-access") {
			t.Errorf("unexpected recovered upload: %s %s, headers %v, body %q", req.Method, req.URL.Path, req.Header, body)
		}
		uploads.Add(1)
		w.Header().Set("ETag", `"test"`)
	})
}

func assertAbandonmentAlert(t *testing.T, eventPath string, alert *UploadAbandonedEvent, req uploadRequest) {
	t.Helper()
	if alert.Filename != filepath.Base(req.localPath) || alert.S3Key != req.s3Key || alert.RetryCount != 3 || alert.LastError == "" {
		t.Fatalf("abandonment alert = %+v", alert)
	}
	events, _, err := eventlog.ReadLast(eventPath, 100, 0, eventlog.FilterRecorder)
	if err != nil {
		t.Fatal(err)
	}
	var abandoned int
	for _, event := range events {
		if event.Type == eventlog.UploadAbandoned {
			abandoned++
		}
	}
	if abandoned != 1 {
		t.Fatalf("upload_abandoned events = %d, want 1", abandoned)
	}
}

func TestUploadExpiredWhileOfflineRequeuesOnStartup(t *testing.T) {
	t.Parallel()
	var uploaded atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != "audio" || req.Method != http.MethodPut {
			t.Errorf("unexpected recovered request: %s %q, %v", req.Method, body, err)
		}
		uploaded.Add(1)
		w.Header().Set("ETag", `"test"`)
	}))
	defer server.Close()
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, "offline.mp3", "audio")
	pending := pendingUpload{
		request:      uploadRequest{localPath: file, s3Key: "recordings/Test/offline.mp3", fileSize: 5, deleteAfterUpload: true},
		firstAttempt: time.Now().Add(-MaxUploadRetryAge - time.Hour), retryCount: 3,
	}
	if err := r.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager("", r.spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	restoredAfter := time.Now()
	if err := restarted.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	restored := restarted.recorders[cfg.ID].retryQueue[0]
	if restored.retryCount != 0 || restored.firstAttempt.Before(restoredAfter) || restored.firstAttempt.After(time.Now()) {
		t.Fatalf("startup did not reset the expired retry window: %+v", restored)
	}
	restarted.processUploadRetryQueues()
	if uploaded.Load() != 1 {
		t.Fatal("expired, unmarked metadata was abandoned again instead of re-queued")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("successfully recovered spool file remains: %v", err)
	}
}

func TestAbandonedUploadRequeuesAfterClockRollback(t *testing.T) {
	t.Parallel()
	cfg := testS3Recorder()
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, "rollback.mp3", "audio")
	// A boot clock behind the abandonment makes the retry window look unexpired.
	pending := pendingUpload{
		request:      uploadRequest{localPath: file, s3Key: "recordings/Test/rollback.mp3", fileSize: 5, deleteAfterUpload: true},
		firstAttempt: time.Now().Add(-time.Hour), retryCount: 3, abandoned: true,
	}
	if err := r.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager("", r.spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := restarted.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if restored := restarted.recorders[cfg.ID].retryQueue[0]; restored.retryCount != 0 || restored.firstAttempt.Before(before) || restored.firstAttempt.After(after) {
		t.Fatalf("startup did not reset the abandoned retry window: %+v", restored)
	}
}
