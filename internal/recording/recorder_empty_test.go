package recording

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestEmptyRecordingsAreDiscardedButSilentAudioIsKept(t *testing.T) {
	neverRotate(t)
	// MP3 can produce a nonempty header even when it received no PCM.
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	for _, mode := range []types.StorageMode{types.StorageLocal, types.StorageBoth, types.StorageS3} {
		t.Run(string(mode), func(t *testing.T) {
			pcm := make([]byte, 4096) // Digital silence is real audio, not an empty run.
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				if req.Method == http.MethodPut {
					uploads.Add(1)
					if !bytes.Equal(body, pcm) {
						t.Errorf("uploaded %q, want silent PCM", body)
					}
				}
				w.Header().Set("ETag", `"test"`)
			}))
			defer server.Close()
			cfg := testS3Recorder()
			cfg.StorageMode, cfg.LocalPath, cfg.S3Endpoint = mode, t.TempDir(), server.URL
			r, eventPath := errorTestRecorder(t, cfg)
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			r.mu.RLock()
			emptyRotation := r.currentFile
			r.mu.RUnlock()
			r.rotateFile()
			assertNoRecording(t, emptyRotation)
			r.mu.RLock()
			withAudio := r.currentFile
			r.mu.RUnlock()
			r.WriteAudio(pcm)
			r.rotateFile()
			r.mu.RLock()
			emptyStop := r.currentFile
			r.mu.RUnlock()
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			assertNoRecording(t, emptyStop)
			wantUploads := int32(1)
			if mode == types.StorageLocal {
				wantUploads = 0
			}
			if got := uploads.Load(); got != wantUploads || r.PendingUploadCount() != 0 {
				t.Errorf("uploads = %d, pending = %d; want %d, 0", got, r.PendingUploadCount(), wantUploads)
			}
			if mode != types.StorageS3 {
				data, err := os.ReadFile(withAudio) //nolint:gosec // Test temp recording.
				if err != nil || !bytes.Equal(data, pcm) {
					t.Fatalf("partial hour = %q, %v; want silent PCM preserved", data, err)
				}
			}
			events, _, err := eventlog.ReadLast(eventPath, 100, 0, eventlog.FilterRecorder)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type != eventlog.UploadQueued {
					continue
				}
				details, ok := event.Details.(map[string]any)
				if !ok || details["filename"] != filepath.Base(withAudio) {
					t.Errorf("empty recording queued: %+v", event)
				}
			}
		})
	}
}

func assertNoRecording(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty recording %s still exists: %v", filepath.Base(path), err)
	}
}

func TestEmptyFailedFinalizePreservesOutputForInspection(t *testing.T) {
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	r, eventPath := errorTestRecorder(t, localOnDemandRecorder(t, "failed-empty"))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	r.mu.RLock()
	file := r.currentFile
	r.mu.RUnlock()
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "header without audio" { //nolint:gosec // Test temp recording.
		t.Fatalf("failed output = %q, %v; want preserved", data, err)
	}
	assertRecorderErrorEvent(t, eventPath, filepath.Base(file), "No space left on device")
}
