package recording

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestFailedRecordingIsNotRecoveredAfterRestart(t *testing.T) {
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	for _, mode := range []types.StorageMode{types.StorageLocal, types.StorageBoth, types.StorageS3} {
		t.Run(string(mode), func(t *testing.T) {
			for _, audio := range []bool{false, true} {
				name := "empty"
				if audio {
					name = "partial audio"
				}
				t.Run(name, func(t *testing.T) {
					store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						t.Error("failed output attempted an upload")
						w.WriteHeader(http.StatusForbidden)
					}))
					t.Cleanup(store.Close)
					cfg := testS3Recorder()
					cfg.RecordingMode, cfg.S3Endpoint = types.RecordingOnDemand, store.URL
					cfg.StorageMode, cfg.LocalPath = mode, t.TempDir()
					r, events := errorTestRecorder(t, cfg)
					file := startCaptureTestRecorder(t, r)
					want := []byte("header without audio")
					if audio {
						want = bytes.Repeat([]byte{1, 2, 3, 4}, 1024)
						r.WriteAudio(want)
					}
					r.Stop()
					assertRecordedCapture(t, file, want)
					assertRecordingQuarantined(t, r, file)
					assertSingleRecorderError(t, r, events, file, "No space left on device")
					if tk, _ := ticketFor(r.outbox, file); !strings.Contains(tk.LastError, "No space left on device") {
						t.Fatalf("failed ticket does not hold the reason: %q", tk.LastError)
					}
					restarted := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
					deliverDue(t, restarted)
					if got := pendingTickets(restarted); got != 0 {
						t.Fatalf("failed output queued after restart: %d uploads", got)
					}
				})
			}
		})
	}
}

func assertSingleRecorderError(t *testing.T, r *GenericRecorder, eventsPath, file, cause string) {
	t.Helper()
	assertRecorderErrorEvent(t, eventsPath, filepath.Base(file), cause)
	r.events.flush() // Count every error the operation queued.
	events, _, err := eventlog.ReadLast(eventsPath, 100, 0, eventlog.FilterRecorder)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == eventlog.RecorderError {
			details, ok := event.Details.(map[string]any)
			if ok && details["filename"] == filepath.Base(file) {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("recorder_error count for %s = %d, want 1", filepath.Base(file), count)
	}
}

func TestFailedEmptyRemovalStaysQuarantinedAfterRestart(t *testing.T) {
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.S3Endpoint = types.RecordingOnDemand, "http://127.0.0.1:1"
	r, events := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	originalRemove := removeEmptyRecording
	removeEmptyRecording = func(path string) error { return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission} }
	t.Cleanup(func() { removeEmptyRecording = originalRemove })
	r.Stop()
	assertRecordingQuarantined(t, r, file)
	assertSingleRecorderError(t, r, events, file, "remove empty recording")
	restarted := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
	if pendingTickets(restarted) != 0 {
		t.Fatal("failed empty-file removal became an automatic upload after restart")
	}
}

func TestWriterTimeoutReportsOneErrorAndQuarantinesPartialAudio(t *testing.T) {
	t.Setenv(fakeFFmpegFailWriteEnv, "1")
	t.Setenv(fakeFFmpegBlockWriterEnv, "1")
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	originalTimeout := writerDrainTimeout
	writerDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { writerDrainTimeout = originalTimeout })
	r, events := errorTestRecorder(t, localOnDemandRecorder(t, "timeout"))
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio(bytes.Repeat([]byte{1, 2, 3, 4}, 256*1024))
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := os.Stat(file)
		if err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake encoder did not reach the blocked-write phase")
		}
		time.Sleep(time.Millisecond)
	}
	r.Stop()
	assertSingleRecorderError(t, r, events, file, "writer timed out")
	assertRecordingQuarantined(t, r, file)
	assertRecordedCapture(t, file, bytes.Repeat([]byte{1, 2, 3, 4}, 1024))
}

func TestTicketReservesNameWithoutAnAudioFile(t *testing.T) {
	cfg := localOnDemandRecorder(t, "ticket-collision")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	for _, at := range []time.Time{time.Now(), time.Now().Add(time.Minute)} {
		// A failed recording whose file is gone still owns its name.
		if err := r.outbox.newTicket(&ticket{Path: filepath.Join(cfg.LocalPath, r.generateFilename(at))}); err != nil {
			t.Fatal(err)
		}
	}
	file := startCaptureTestRecorder(t, r)
	if !strings.HasSuffix(file, "-2.ts") {
		t.Fatalf("reused a ticket's filename: %s", file)
	}
	r.Stop()
	if _, ok := ticketFor(r.outbox, file); ok {
		t.Fatal("a successful empty run left its ticket")
	}
}

func TestTicketCreationFailureDoesNotStartEncoder(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop root or Windows")
	}
	cfg := localOnDemandRecorder(t, "ticket-create-failure")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	if err := os.Chmod(r.outbox.dir, 0o500); err != nil { //nolint:gosec // A test dir must stay traversable.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.outbox.dir, 0o700) }) //nolint:gosec // Restore the test dir for cleanup.
	startSettled(t, r)
	if status := r.Status(); status.State != types.ProcessError || !strings.Contains(status.Error, "ticket") || r.currentSegment() != nil {
		t.Fatalf("ticket failure did not prevent encoding: %+v", status)
	}
	if entries, err := os.ReadDir(cfg.LocalPath); err != nil || len(entries) != 0 {
		t.Fatalf("failed ticket creation left an unowned output: %v, %v", entries, err)
	}
}

func TestFailedTicketWriteStillQuarantinesOutput(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop root or Windows")
	}
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.StorageMode, cfg.LocalPath = types.RecordingOnDemand, types.StorageBoth, t.TempDir()
	r, _ := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio([]byte{1, 2, 3, 4})
	// The outbox cannot record the failure; the marker beside the file must.
	if err := os.Chmod(r.outbox.dir, 0o500); err != nil { //nolint:gosec // A test dir must stay traversable.
		t.Fatal(err)
	}
	r.Stop()
	if err := os.Chmod(r.outbox.dir, 0o700); err != nil { //nolint:gosec // Restore the test dir.
		t.Fatal(err)
	}
	assertRecordedCapture(t, file, []byte{1, 2, 3, 4})
	assertRecordingQuarantined(t, r, file)
}

func TestDiscardMarksAReplacedReservationAsFailed(t *testing.T) {
	cfg := localOnDemandRecorder(t, "replaced")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg})
	path := filepath.Join(cfg.LocalPath, "reserved.ts")
	output, err := r.reserveRecordingOutput(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		_ = output.file.Close()
		t.Skipf("the platform protects the open reservation: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.discard()
	assertRecordingQuarantined(t, r, path)
	assertRecordedCapture(t, path, []byte("replacement"))
	if _, err := output.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reservation descriptor not closed: %v", err)
	}
}

func TestRecorderIDCannotBeReaddedWhileItsRemovalStops(t *testing.T) {
	t.Parallel()
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testS3Recorder()
	m.mu.Lock()
	m.removing[cfg.ID] = struct{}{} // RemoveRecorder is still stopping the old run.
	m.mu.Unlock()
	if err := m.AddRecorder(cfg); err == nil {
		t.Fatal("re-added a recorder whose spool still holds the removed run's output")
	}
	if err := m.RemoveRecorder("missing"); err == nil {
		t.Fatal("removing an unknown recorder succeeded")
	}
	m.mu.Lock()
	delete(m.removing, cfg.ID)
	m.mu.Unlock()
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.removing) != 0 {
		t.Fatalf("finished removal still blocks its ID: %v", m.removing)
	}
}
