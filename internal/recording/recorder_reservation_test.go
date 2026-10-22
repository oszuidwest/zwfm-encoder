package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecordersReserveDifferentFilesBeforeEncoding(t *testing.T) {
	cfg := localOnDemandRecorder(t, "same-name")
	second := *cfg
	second.ID = "second"
	a := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	b := newTestRecorder(t, GenericRecorderConfig{Recorder: &second, FFmpegPath: os.Args[0]})
	files := make([]string, 2)
	for i, r := range []*GenericRecorder{a, b} {
		t.Cleanup(func() { _ = r.Stop() })
		files[i] = startCaptureTestRecorder(t, r)
	}
	if files[0] == files[1] {
		t.Fatal("two live recorders claimed the same output file")
	}
	for i, r := range []*GenericRecorder{a, b} {
		pcm := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		r.WriteAudio(pcm)
		if err := r.Stop(); err != nil {
			t.Fatal(err)
		}
		assertRecordedCapture(t, files[i], pcm)
	}
}

func TestReservationCollisionKeepsConcurrentForeignFile(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		name := "file"
		if symlink {
			name = "dangling symlink"
		}
		t.Run(name, func(t *testing.T) {
			if symlink {
				probe := filepath.Join(t.TempDir(), "probe")
				if err := os.Symlink(probe+"-target", probe); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			cfg := localOnDemandRecorder(t, "collision")
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
			originalReserve := reserveRecordingFile
			var foreign string
			target := filepath.Join(t.TempDir(), "outside.ts")
			// Simulate an entry appearing immediately before the atomic create, not
			// before a separate existence check. The recorder must retry rather than
			// adopt it or follow it.
			first := true
			reserveRecordingFile = func(path string) (*os.File, error) {
				if first {
					first = false
					foreign = path
					if symlink {
						if err := os.Symlink(target, path); err != nil {
							return nil, err
						}
					} else if err := os.WriteFile(path, []byte("foreign audio"), 0o600); err != nil { //nolint:gosec // Test temp recording.
						return nil, err
					}
				}
				return originalReserve(path)
			}
			t.Cleanup(func() { _ = r.Stop(); reserveRecordingFile = originalReserve })
			path := startCaptureTestRecorder(t, r)
			pcm := []byte{1, 2, 3, 4}
			r.WriteAudio(pcm)
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			if path == foreign || !strings.HasSuffix(path, "-2.ts") {
				t.Fatalf("collision was not retried: output=%s, foreign=%s", path, foreign)
			}
			assertRecordedCapture(t, path, pcm)
			if symlink {
				assertNoRecording(t, target) // The link was not followed.
			} else {
				assertRecordedCapture(t, foreign, []byte("foreign audio"))
			}
		})
	}
}

func TestStopDuringReservationRemovesUnstartedOutput(t *testing.T) {
	cfg := localOnDemandRecorder(t, "stop-reservation")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	r.state, r.startGen = types.ProcessStarting, 1
	originalReserve := reserveRecordingFile
	reserved, blocked, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	reserveRecordingFile = func(path string) (*os.File, error) {
		file, err := originalReserve(path)
		close(reserved)
		<-blocked
		return file, err
	}
	go func() { defer close(done); r.startAsync(1) }()
	t.Cleanup(func() { release(); <-done; reserveRecordingFile = originalReserve; _ = r.Stop() })
	select {
	case <-reserved:
	case <-time.After(3 * time.Second):
		t.Fatal("output was not reserved")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- r.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop waited for the blocked reservation")
	}
	release()
	<-done
	if entries, err := os.ReadDir(cfg.LocalPath); err != nil || len(entries) != 0 {
		t.Fatalf("obsolete reservation left files: %v, %v", entries, err)
	}
	if r.Status().State != types.ProcessStopped || r.result != nil {
		t.Fatal("obsolete reservation started an encoder after Stop")
	}
}

func TestReservationErrorDoesNotLaunchEncoder(t *testing.T) {
	cfg := localOnDemandRecorder(t, "permission-error")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	r.state, r.startGen = types.ProcessStarting, 1
	originalReserve := reserveRecordingFile
	var calls atomic.Int32
	reserveRecordingFile = func(path string) (*os.File, error) {
		if calls.Add(1) > 1 {
			t.Error("a non-collision error was retried under another name")
			return originalReserve(path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
	}
	t.Cleanup(func() { reserveRecordingFile = originalReserve; _ = r.Stop() })
	r.startAsync(1)
	if status := r.Status(); status.State != types.ProcessError || !strings.Contains(status.Error, os.ErrPermission.Error()) || r.result != nil {
		t.Fatalf("reservation failure was ignored: %+v", status)
	}
}

func TestReleaseReportsDisappearedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserved.ts")
	file, err := reserveRecordingFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output := &recordingOutput{file: file}
	if err := os.Remove(path); err != nil {
		_ = file.Close()
		if runtime.GOOS == "windows" {
			t.Skipf("Windows protects the open reservation from removal: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := output.release(); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("release of a removed output = %v, want a disappeared error", err)
	}
}

func TestFailedEncoderStartReleasesReservation(t *testing.T) {
	cfg := localOnDemandRecorder(t, "failed-start")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: filepath.Join(t.TempDir(), "missing-ffmpeg")})
	r.state, r.startGen = types.ProcessStarting, 1
	r.startAsync(1) // Returns after the failed-start cleanup.
	if r.Status().State != types.ProcessError {
		t.Fatalf("missing FFmpeg did not fail the start: %+v", r.Status())
	}
	if entries, err := os.ReadDir(cfg.LocalPath); err != nil || len(entries) != 0 {
		t.Fatalf("failed startup left reserved files: %v, %v", entries, err)
	}
}

func TestReservationRejectsTraversalAtFileOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	path := allowed + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside.ts" //nolint:gocritic // Preserve the raw traversal input under test.
	file, err := reserveRecordingFile(path)
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("raw traversal accepted at file-open boundary")
	}
	assertNoRecording(t, filepath.Join(dir, "outside.ts"))
}
