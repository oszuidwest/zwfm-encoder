package recording

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestReplacedRecordingIsNotUploadedOrRemoved(t *testing.T) {
	for _, audio := range []bool{false, true} {
		name := "empty"
		if audio {
			name = "audio"
		}
		t.Run(name, func(t *testing.T) {
			endpoint, uploads := conditionalPutServer(t, "", func(http.ResponseWriter, string) bool { return false })
			cfg := testS3Recorder()
			cfg.RecordingMode, cfg.StorageMode = types.RecordingOnDemand, types.StorageBoth
			cfg.LocalPath, cfg.S3Endpoint = t.TempDir(), endpoint
			r, path := errorTestRecorder(t, cfg)
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			seg := r.currentSegment()
			file, result := seg.file, seg.result
			if audio {
				r.WriteAudio(make([]byte, 4096))
				deadline := time.Now().Add(3 * time.Second)
				for result.BytesWritten() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if result.BytesWritten() == 0 {
					t.Fatal("encoder did not accept test audio")
				}
			}
			result.CloseStdin()
			if err := result.Wait(); err != nil {
				t.Fatal(err)
			}
			// Replace the inode after FFmpeg exits but before finalization claims it.
			other := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(other, []byte("pre-existing recording"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(file); err != nil { //nolint:gosec // Test temp recording.
				if runtime.GOOS == "windows" {
					t.Skipf("Windows protects the open reservation from replacement: %v", err)
				}
				t.Fatal(err)
			}
			if err := os.Rename(other, file); err != nil { //nolint:gosec // Test temp recording.
				t.Fatal(err)
			}
			r.Stop()
			if got := len(uploads()); got != 0 {
				t.Errorf("uploaded unrelated replacement %d times", got)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "pre-existing recording" { //nolint:gosec // Test temp output.
				t.Fatalf("pre-existing recording = %q, %v; want untouched", data, err)
			}
			assertRecorderErrorEvent(t, path, filepath.Base(file), "replaced")
		})
	}
}

func TestFailedFinalizeReportsErrorAndKeepsPartialAudio(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	logs := captureRecorderErrors(t)
	cfg := localOnDemandRecorder(t, "finalize-error")
	cfg.RecordingMode = types.RecordingHourly
	r, path := errorTestRecorder(t, cfg)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	file := r.currentFile()
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 4096)
	r.WriteAudio(pcm)
	t.Setenv(fakeFFmpegFailFinalizeEnv, "")
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || !strings.Contains(status.Error, "No space left on device") {
		t.Fatalf("failed finalize stopped recording or lost its warning: %+v", status)
	}
	if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, pcm) { //nolint:gosec // Test temp output.
		t.Fatalf("partial recording = %d bytes, %v; want %d preserved bytes", len(data), err, len(pcm))
	}
	assertSingleRecorderError(t, path, file, "No space left on device")
	assertRecorderErrorLog(t, logs, filepath.Base(file))
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || status.Error != "" {
		t.Fatalf("successful rotation did not clear previous finalize warning: %+v", status)
	}
	r.Stop()
	assertRecordingQuarantined(t, r, file)
}

func assertRecorderErrorLog(t *testing.T, logs chan slog.Record, wantFile string) {
	t.Helper()
	for len(logs) > 0 {
		record := <-logs
		var filename string
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "file" {
				filename = attr.Value.String()
			}
			return true
		})
		if record.Level >= slog.LevelError && filename == wantFile {
			return
		}
	}
	t.Fatal("finalize failure was not logged at Error with its filename")
}

func TestFailedRotationStartEmitsRecorderError(t *testing.T) {
	neverRotate(t)
	logs := captureRecorderErrors(t)
	cfg := localOnDemandRecorder(t, "rotation-error")
	cfg.RecordingMode = types.RecordingHourly
	r, path := errorTestRecorder(t, cfg)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	previous := r.currentFile()
	r.ffmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	rotateTestRecorder(r)
	details := assertRecorderErrorEvent(t, path, "", "missing-ffmpeg")
	filename, ok := details["filename"].(string)
	if !ok || filename == "" || filename == filepath.Base(previous) {
		t.Fatalf("rotation failure filename = %q, want attempted new output, not %q", filename, filepath.Base(previous))
	}
	assertRecorderErrorLog(t, logs, filename)
	assertNoRecording(t, filepath.Join(cfg.LocalPath, filename)) // The unstarted reservation.
}

func TestWriteErrorQuarantinesPartialAudioWithoutUpload(t *testing.T) {
	t.Setenv(fakeFFmpegFailWriteEnv, "1")
	endpoint, uploads := conditionalPutServer(t, "", func(http.ResponseWriter, string) bool { return false })
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.S3Endpoint = types.RecordingOnDemand, endpoint
	r, eventPath := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	writeUntilRecorderLeavesRunning(t, r)
	r.Stop()
	assertRecordedCapture(t, file, make([]byte, 4096))
	assertRecordingQuarantined(t, r, file)
	assertSingleRecorderError(t, eventPath, file, "simulated write failure")
	if pendingTickets(r) != 0 || len(uploads()) != 0 {
		t.Fatalf("failed output queued (%d) or uploaded (%d)", pendingTickets(r), len(uploads()))
	}
}

func TestWriteErrorKeepsReplacedRecording(t *testing.T) {
	t.Setenv(fakeFFmpegFailWriteEnv, "1")
	endpoint, uploads := conditionalPutServer(t, "", func(http.ResponseWriter, string) bool { return false })
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.StorageMode = types.RecordingOnDemand, types.StorageBoth
	cfg.LocalPath, cfg.S3Endpoint = t.TempDir(), endpoint
	r, path := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	result := r.currentSegment().result
	r.WriteAudio(make([]byte, 4096))
	_ = result.Wait() // The fake exits after the first 4096 bytes.
	other := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(other, []byte("pre-existing recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows protects the open reservation from replacement: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Rename(other, file); err != nil {
		t.Fatal(err)
	}
	writeUntilRecorderLeavesRunning(t, r)
	r.Stop()
	if n := pendingTickets(r); n != 0 {
		t.Fatalf("replacement registered for recovery: %d", n)
	}
	if got := len(uploads()); got != 0 {
		t.Errorf("uploaded unrelated replacement %d times", got)
	}
	assertRecordedCapture(t, file, []byte("pre-existing recording"))
	assertRecorderErrorEvent(t, path, filepath.Base(file), "replaced")
}

func TestWriteErrorQuarantinesAudioDespiteZeroAcceptedBytes(t *testing.T) {
	// A failed Windows pipe write can report zero accepted bytes after FFmpeg
	// consumed PCM; the fake models it by exiting before reading stdin.
	t.Setenv(fakeFFmpegFailWriteEnv, "empty")
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.S3Endpoint = types.RecordingOnDemand, "http://127.0.0.1:1" // Never reach real S3.
	r, _ := errorTestRecorder(t, cfg)
	file := startCaptureTestRecorder(t, r)
	result := r.currentSegment().result
	_ = result.Wait()
	writeUntilRecorderLeavesRunning(t, r)
	r.Stop()
	if result.BytesWritten() != 0 {
		t.Fatalf("pipe accepted %d bytes, want 0", result.BytesWritten())
	}
	assertRecordedCapture(t, file, []byte("header without audio"))
	assertRecordingQuarantined(t, r, file)
	if pendingTickets(r) != 0 {
		t.Fatal("quarantined output registered for upload")
	}
}

// writeUntilRecorderLeavesRunning feeds audio until a write error stops r.
func writeUntilRecorderLeavesRunning(t *testing.T, r *GenericRecorder) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for r.Status().State == types.ProcessRunning {
		if time.Now().After(deadline) {
			t.Fatal("write error did not stop the recorder")
		}
		r.WriteAudio(make([]byte, 16384))
		time.Sleep(5 * time.Millisecond)
	}
}

func errorTestRecorder(t *testing.T, cfg *types.Recorder) (recorder *GenericRecorder, eventPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir(), EventLogger: logger})
	t.Cleanup(func() { r.Stop() })
	return r, path
}

// assertRecorderErrorEvent waits for the recorder_error event, which the
// recorder writes after the call that caused it returned.
func assertRecorderErrorEvent(t *testing.T, path, filename, cause string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _, err := eventlog.ReadLast(path, 100, 0, eventlog.FilterRecorder)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			details, ok := event.Details.(map[string]any)
			message, _ := details["error"].(string)
			if ok && event.Type == eventlog.RecorderError && strings.Contains(message, cause) &&
				(filename == "" || details["filename"] == filename) {
				return details
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no recorder_error for %q containing %q: %+v", filename, cause, events)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type recorderErrorCapture struct {
	slog.Handler
	logs chan slog.Record
}

func (h recorderErrorCapture) Handle(_ context.Context, record slog.Record) error { //nolint:gocritic // slog.Handler requires a value parameter.
	if record.Level >= slog.LevelError {
		select {
		case h.logs <- record.Clone():
		default:
		}
	}
	return nil
}

func captureRecorderErrors(t *testing.T) chan slog.Record {
	t.Helper()
	logs := make(chan slog.Record, 100)
	previous := slog.Default()
	slog.SetDefault(slog.New(recorderErrorCapture{Handler: slog.NewTextHandler(io.Discard, nil), logs: logs}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func TestWriteErrorWithEmptyOutputIsNotRecovered(t *testing.T) {
	t.Setenv(fakeFFmpegFailWriteEnv, "empty")
	endpoint, uploads := conditionalPutServer(t, "", func(http.ResponseWriter, string) bool { return false })
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.S3Endpoint = types.RecordingOnDemand, endpoint
	r, _ := errorTestRecorder(t, cfg)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	file := r.currentFile()
	writeUntilRecorderLeavesRunning(t, r)
	r.Stop()
	if n := pendingTickets(r); n != 0 {
		t.Fatalf("empty output registered for recovery: %d", n)
	}
	assertNoRecording(t, file) // An empty placeholder is not left behind,
	if _, ok := ticketFor(r.outbox, file); ok {
		t.Fatal("ticket left for a removed recording")
	}
	if put := len(uploads()); put != 0 {
		t.Fatalf("empty output uploaded %d times", put)
	}
}
