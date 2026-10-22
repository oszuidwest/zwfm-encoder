package recording

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestChangedStorageIsPreparedBeforeEncoderLaunch(t *testing.T) {
	neverRotate(t)
	for _, phase := range []string{"validation", "startup filename", "rotation filename"} {
		t.Run(phase, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cfg := localOnDemandRecorder(t, "storage-change")
			rotation := phase == "rotation filename"
			if rotation {
				cfg.RecordingMode = types.RecordingHourly
			}
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: executable, SpoolDir: t.TempDir()})
			store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("empty storage-change test attempted an upload")
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(store.Close)
			if rotation {
				startCaptureTestRecorder(t, r)
			}
			entered, blocked, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			delay := sync.OnceFunc(func() { close(entered); <-blocked })
			originalCheck, originalReserve := checkRecordingPath, reserveRecordingFile
			checkRecordingPath = func(path string) error {
				if phase == "validation" {
					delay()
					return errors.New("obsolete path validation failed")
				}
				return originalCheck(path)
			}
			reserveRecordingFile = func(path string) (*os.File, error) {
				if phase != "validation" && strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
					delay()
				}
				return originalReserve(path)
			}
			go func() {
				defer close(done)
				if rotation {
					rotateTestRecorder(r)
				} else if err := r.Start(); err != nil {
					t.Error(err)
				}
			}()
			t.Cleanup(func() {
				release()
				<-done
				r.Stop()
				checkRecordingPath, reserveRecordingFile = originalCheck, originalReserve
			})
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("preparation did not reach the blocked filesystem operation")
			}
			updated := *testS3Recorder()
			updated.S3Endpoint = store.URL
			updated.ID, updated.Name, updated.RecordingMode = cfg.ID, cfg.Name, cfg.RecordingMode
			if err := r.UpdateConfig(&updated); err != nil {
				t.Fatal(err)
			}
			release()
			<-done
			waitForRecorderState(t, r, types.ProcessRunning)
			current := r.Config()
			dir := filepath.Join(r.spoolDir, "recordings", current.RecordingFilePrefix())
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("new S3 output directory was not prepared: %v, %v", info, err)
			}
			if status := r.Status(); status.State != types.ProcessRunning || status.Error != "" {
				t.Fatalf("obsolete configuration affected recorder status: %+v", status)
			}
		})
	}
}

func TestRotationDirectoryFailureKeepsCurrentEncoder(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "directory-failure")
	previous := r.currentSegment()
	pcm := []byte{1, 2, 3, 4}
	r.WriteAudio(pcm)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	updated := r.Config()
	updated.LocalPath = blocked
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	r.settle() // The new path is tried at once (D3).
	before := len(capturedTimers())
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || !strings.Contains(status.Error, "writable") {
		t.Fatalf("directory failure stopped current recording or lost its error: %+v", status)
	}
	if rearmed := len(capturedTimers()) - before; rearmed != 1 {
		t.Fatalf("failed rotation armed %d next rotations, want 1: one mount blip must not end hourly rotation", rearmed)
	}
	if r.currentSegment() != previous {
		t.Fatal("failed output preparation detached the active recording")
	}
	r.WriteAudio(pcm)
	r.Stop()
	assertRecordedCapture(t, previous.file, append(pcm, pcm...))
}

func TestRecordingDirectoryRejectsTraversalBeforeCreation(t *testing.T) {
	t.Parallel()
	for _, mode := range []types.StorageMode{types.StorageLocal, types.StorageS3} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			allowed := filepath.Join(root, "allowed")
			if err := os.Mkdir(allowed, 0o700); err != nil {
				t.Fatal(err)
			}
			path := allowed + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside" //nolint:gocritic // filepath.Join would erase the traversal being tested.
			if err := prepareRecordingDir(mode, path); err == nil {
				t.Fatal("directory traversal was accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
				t.Fatalf("rejected directory was created: %v", err)
			}
		})
	}
}

// A new output saved while hourly survives a switch to on-demand that cancels
// the rotation preparing it.
func TestModeSwitchKeepsSavedOutput(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "mode-output")
	saved := r.Config()
	saved.LocalPath = t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	original := reserveRecordingFile
	reserveRecordingFile = func(path string) (*os.File, error) {
		if filepath.Dir(path) == saved.LocalPath {
			close(entered)
			<-release
		}
		return original(path)
	}
	t.Cleanup(func() { unblock(); r.close(); reserveRecordingFile = original })
	if err := r.UpdateConfig(&saved); err != nil {
		t.Fatal(err)
	}
	<-entered // The D3 rotation to the new path is preparing.
	reserveRecordingFile = original
	saved.RecordingMode = types.RecordingOnDemand
	if err := r.UpdateConfig(&saved); err != nil {
		t.Fatal(err)
	}
	unblock()
	r.settle()
	if got := r.currentSegment().cfg.LocalPath; got != saved.LocalPath {
		t.Fatalf("recording in %s, want the saved %s", got, saved.LocalPath)
	}
}
