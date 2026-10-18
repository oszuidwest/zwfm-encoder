package recording

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestEmptyRecordingsAreDiscardedButSilentAudioIsKept(t *testing.T) {
	neverRotate(t)
	// MP3 can produce a nonempty header even when it received no PCM.
	t.Setenv(fakeFFmpegEmptyHeaderEnv, "1")
	for _, mode := range []types.StorageMode{types.StorageLocal, types.StorageBoth, types.StorageS3} {
		t.Run(string(mode), func(t *testing.T) {
			pcm := make([]byte, 4096) // Digital silence is real audio, not an empty run.
			// Only the silent PCM file may arrive; any other body is rejected and stays pending.
			endpoint, uploads := conditionalPutServer(t, string(pcm), func(http.ResponseWriter, string) bool { return false })
			cfg := testS3Recorder()
			cfg.StorageMode, cfg.LocalPath, cfg.S3Endpoint = mode, t.TempDir(), endpoint
			r, _ := errorTestRecorder(t, cfg)
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			r.mu.RLock()
			emptyRotation := r.currentFile
			r.mu.RUnlock()
			rotateTestRecorder(r)
			assertNoRecording(t, emptyRotation)
			r.mu.RLock()
			withAudio := r.currentFile
			r.mu.RUnlock()
			r.WriteAudio(pcm)
			rotateTestRecorder(r)
			r.mu.RLock()
			emptyStop := r.currentFile
			r.mu.RUnlock()
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			assertNoRecording(t, emptyStop)
			wantUploads := 1
			if mode == types.StorageLocal {
				wantUploads = 0
			}
			if got := len(uploads()); got != wantUploads || r.PendingUploadCount() != 0 {
				t.Errorf("uploads = %d, pending = %d; want %d, 0", got, r.PendingUploadCount(), wantUploads)
			}
			if mode != types.StorageS3 {
				data, err := os.ReadFile(withAudio) //nolint:gosec // Test temp recording.
				if err != nil || !bytes.Equal(data, pcm) {
					t.Fatalf("partial hour = %q, %v; want silent PCM preserved", data, err)
				}
			}
		})
	}
}

func assertNoRecording(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("recording output %s still exists: %v", filepath.Base(path), err)
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

func TestFailedFinalizeRemovesUntouchedPlaceholder(t *testing.T) {
	t.Setenv(fakeFFmpegFailFinalizeEnv, "1")
	r, eventPath := errorTestRecorder(t, localOnDemandRecorder(t, "failed-untouched"))
	file := startCaptureTestRecorder(t, r)
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	assertNoRecording(t, file)
	assertNoRecording(t, file+unfinalizedRecordingSuffix)
	assertRecorderErrorEvent(t, eventPath, filepath.Base(file), "No space left on device")
}
