package recording

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
					if err := r.Stop(); err != nil {
						t.Fatal(err)
					}
					assertRecordedCapture(t, file, want)
					assertRecordingQuarantined(t, file)
					assertSingleRecorderError(t, events, file, "No space left on device")
					restarted := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
					if err := restarted.reconcilePendingUploads(); err != nil {
						t.Fatal(err)
					}
					if got := restarted.PendingUploadCount(); got != 0 {
						t.Fatalf("failed output queued after restart: %d uploads", got)
					}
					restarted.queueForUpload(file)
					if restarted.PendingUploadCount() != 0 {
						t.Fatal("quarantine bypassed by direct upload queueing")
					}
					restarted.processRetryQueue()
				})
			}
		})
	}
}

func assertRecordingQuarantined(t *testing.T, file string) {
	t.Helper()
	if _, err := os.Stat(file + unfinalizedRecordingSuffix); err != nil {
		t.Fatalf("unfinalized recording marker missing: %v", err)
	}
}

func assertSingleRecorderError(t *testing.T, eventsPath, file, cause string) {
	t.Helper()
	assertRecorderErrorEvent(t, eventsPath, filepath.Base(file), cause)
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
	cfg.RecordingMode = types.RecordingOnDemand
	r, events := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	originalRemove := removeEmptyRecording
	removeEmptyRecording = func(path string) error { return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission} }
	t.Cleanup(func() { removeEmptyRecording = originalRemove })
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertRecordingQuarantined(t, file)
	assertSingleRecorderError(t, events, file, "remove empty recording")
	restarted := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
	if err := restarted.reconcilePendingUploads(); err != nil {
		t.Fatal(err)
	}
	if restarted.PendingUploadCount() != 0 {
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
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertSingleRecorderError(t, events, file, "writer timed out")
	assertRecordingQuarantined(t, file)
	assertRecordedCapture(t, file, bytes.Repeat([]byte{1, 2, 3, 4}, 1024))
}

func TestFailedMarkerRemovalDoesNotQueueCompletedAudio(t *testing.T) {
	cfg := testS3Recorder()
	cfg.RecordingMode = types.RecordingOnDemand
	r, events := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	r.WriteAudio([]byte{1, 2, 3, 4})
	marker := file + unfinalizedRecordingSuffix
	if err := os.Remove(marker); err != nil { //nolint:gosec // Test temp marker.
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o700); err != nil { //nolint:gosec // Test temp marker.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, "hold"), nil, 0o600); err != nil { //nolint:gosec // Test temp marker.
		t.Fatal(err)
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertRecordingQuarantined(t, file)
	assertSingleRecorderError(t, events, file, "clear unfinalized")
	if r.PendingUploadCount() != 0 {
		t.Fatal("completed output queued despite failed quarantine release")
	}
}

func TestRunningFinalizeWarningClearsOnNextSuccessfulRotation(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	r := startHourlyTestRecorder(t, "warning")
	r.WriteAudio([]byte{1, 2, 3, 4})
	t.Setenv(fakeFFmpegFailFinalizeEnv, "")
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || !strings.Contains(status.Error, "No space left on device") {
		t.Fatalf("running recorder lost finalize warning: %+v", status)
	}
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || status.Error != "" {
		t.Fatalf("successful rotation did not clear previous finalize warning: %+v", status)
	}
}

func TestMissingMarkerCannotMakeLegacyEmptySpoolFileUploadable(t *testing.T) {
	cfg := testS3Recorder()
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, cfg.ID, "Test-2026-09-28-14-37.mp3", "")
	if err := r.reconcilePendingUploads(); err != nil {
		t.Fatal(err)
	}
	if r.PendingUploadCount() != 0 {
		t.Fatal("zero-byte unmarked legacy output was queued")
	}
	if _, ok := r.prepareUploadRequest(file); ok {
		t.Fatal("zero-byte output passed upload validation")
	}
	_, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery deleted retained legacy output")
	}
}

func TestQuarantineAlsoBlocksPreviouslyPersistedUploadMetadata(t *testing.T) {
	cfg := testS3Recorder()
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, cfg.ID, "failed.mp3", "partial audio")
	marker := file + unfinalizedRecordingSuffix
	if err := os.WriteFile(marker, nil, 0o600); err != nil { //nolint:gosec // Test temp marker.
		t.Fatal(err)
	}
	pending := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/Test/failed.mp3", fileSize: 13}}
	if err := r.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcilePendingUploads(); err != nil {
		t.Fatal(err)
	}
	if r.PendingUploadCount() != 0 {
		t.Fatal("upload metadata bypassed the output's quarantine")
	}
	assertRecordedCapture(t, file, []byte("partial audio"))
	if _, err := os.Stat(pending.metadataPath); err != nil {
		t.Fatalf("recovery removed quarantined upload metadata: %v", err)
	}
}

func TestExistingMarkerReservesNameWithoutAnAudioFile(t *testing.T) {
	cfg := localOnDemandRecorder(t, "marker-collision")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	var markers []string
	for _, at := range []time.Time{time.Now(), time.Now().Add(time.Minute)} {
		marker := filepath.Join(cfg.LocalPath, r.generateFilename(at)) + unfinalizedRecordingSuffix
		if err := os.WriteFile(marker, []byte("existing quarantine"), 0o600); err != nil {
			t.Fatal(err)
		}
		markers = append(markers, marker)
	}
	file := startCaptureTestRecorder(t, r)
	if !strings.HasSuffix(file, "-2.ts") {
		t.Fatalf("reused a quarantined filename: %s", file)
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		assertRecordedCapture(t, marker, []byte("existing quarantine"))
	}
	if _, err := os.Stat(file + unfinalizedRecordingSuffix); !os.IsNotExist(err) {
		t.Fatalf("successful empty run left its own marker: %v", err)
	}
}

func TestMarkerCreationFailureDoesNotStartEncoder(t *testing.T) {
	// Output names now depend on IDs, not display-name length. Inject the
	// filesystem failure rather than depending on the old filename format.
	cfg := localOnDemandRecorder(t, "marker-create-failure")
	originalOpen := openRecordingMarker
	openRecordingMarker = func(path string, _ int, _ os.FileMode) (*os.File, error) {
		return nil, &os.PathError{Op: "create", Path: path, Err: os.ErrPermission}
	}
	t.Cleanup(func() { openRecordingMarker = originalOpen })
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	r.state, r.startGen = types.ProcessStarting, 1
	r.startAsync(1)
	if status := r.Status(); status.State != types.ProcessError || !strings.Contains(status.Error, "marker") || r.result != nil {
		t.Fatalf("marker failure did not prevent encoding: %+v", status)
	}
	if entries, err := os.ReadDir(cfg.LocalPath); err != nil || len(entries) != 0 {
		t.Fatalf("failed marker creation left an unowned output: %v, %v", entries, err)
	}
}
