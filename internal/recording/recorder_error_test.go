package recording

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRefusedRecordingIsNotUploaded(t *testing.T) {
	t.Setenv(fakeFFmpegRefuseEnv, "1")
	for _, termination := range []string{"write failure", "stop"} {
		t.Run(termination, func(t *testing.T) {
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				if req.Method == http.MethodPut {
					uploads.Add(1)
				}
				w.Header().Set("ETag", `"test"`)
			}))
			defer server.Close()
			cfg := testS3Recorder()
			cfg.RecordingMode, cfg.StorageMode = types.RecordingOnDemand, types.StorageBoth
			cfg.LocalPath, cfg.S3Endpoint = t.TempDir(), server.URL
			r, path := errorTestRecorder(t, cfg)
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			r.mu.RLock()
			file := r.currentFile
			result := r.result
			r.mu.RUnlock()
			if termination == "stop" {
				r.WriteAudio(make([]byte, 4096))
				if err := result.Wait(); err != nil {
					t.Fatalf("fake refusal should exit 0: %v", err)
				}
				if result.BytesWritten() == 0 {
					t.Fatal("refusal must be detected even after accepting audio")
				}
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) && !strings.Contains(r.Status().Error, "already exists") {
					r.WriteAudio(make([]byte, 16384))
					time.Sleep(5 * time.Millisecond)
				}
				waitForRecorderState(t, r, types.ProcessError)
			}
			message := r.Status().Error
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			if termination == "write failure" && !strings.Contains(message, "already exists") {
				t.Errorf("recorder error = %q, want FFmpeg's refusal", message)
			}
			if got := uploads.Load(); got != 0 {
				t.Errorf("uploaded refused file %d times", got)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "another recording" { //nolint:gosec // Test temp output.
				t.Fatalf("pre-existing recording = %q, %v; want untouched", data, err)
			}
			assertRecorderErrorEvent(t, path, filepath.Base(file), "already exists")
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
	r.mu.RLock()
	file := r.currentFile
	r.mu.RUnlock()
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 4096)
	r.WriteAudio(pcm)
	t.Setenv(fakeFFmpegFailFinalizeEnv, "")
	r.rotateFile()
	if r.Status().State != types.ProcessRunning {
		t.Fatal("failed finalize stopped the next hourly recording")
	}
	if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, pcm) { //nolint:gosec // Test temp output.
		t.Fatalf("partial recording = %d bytes, %v; want %d preserved bytes", len(data), err, len(pcm))
	}
	assertRecorderErrorEvent(t, path, filepath.Base(file), "No space left on device")
	for len(logs) > 0 {
		record := <-logs
		var filename string
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "file" {
				filename = attr.Value.String()
			}
			return true
		})
		if record.Level >= slog.LevelError && filename == filepath.Base(file) {
			return
		}
	}
	t.Fatal("finalize failure was not logged at Error with its filename")
}

func TestFailedRotationStartEmitsRecorderError(t *testing.T) {
	neverRotate(t)
	cfg := localOnDemandRecorder(t, "rotation-error")
	cfg.RecordingMode = types.RecordingHourly
	r, path := errorTestRecorder(t, cfg)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	r.mu.Lock()
	r.ffmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	r.mu.Unlock()
	r.rotateFile()
	assertRecorderErrorEvent(t, path, "", "missing-ffmpeg")
}

func errorTestRecorder(t *testing.T, cfg *types.Recorder) (recorder *GenericRecorder, eventPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir(), EventLogger: logger})
	t.Cleanup(func() { _ = r.Stop() })
	return r, path
}

func assertRecorderErrorEvent(t *testing.T, path, filename, cause string) {
	t.Helper()
	events, _, err := eventlog.ReadLast(path, 100, 0, eventlog.FilterRecorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != eventlog.RecorderError {
			continue
		}
		details, ok := event.Details.(map[string]any)
		if !ok {
			t.Fatalf("error details = %T", event.Details)
		}
		message, _ := details["error"].(string)
		if strings.Contains(message, cause) && (filename == "" || details["filename"] == filename) {
			return
		}
	}
	t.Fatalf("no recorder_error for %q containing %q: %+v", filename, cause, events)
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
