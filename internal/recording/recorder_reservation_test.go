package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecordersReserveDifferentFilesBeforeEncoding(t *testing.T) {
	neverRotate(t)
	cfg := localOnDemandRecorder(t, "same-name")
	second := *cfg
	second.ID = "second"
	a := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	b := NewGenericRecorder(GenericRecorderConfig{Recorder: &second, FFmpegPath: os.Args[0]})
	for _, r := range []*GenericRecorder{a, b} {
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Stop() })
		waitForRecorderState(t, r, types.ProcessRunning)
	}
	if a.currentFile == b.currentFile {
		t.Fatal("two live recorders claimed the same output file")
	}
	for i, r := range []*GenericRecorder{a, b} {
		pcm := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		r.WriteAudio(pcm)
		if err := r.Stop(); err != nil {
			t.Fatal(err)
		}
		assertRecordedCapture(t, r.currentFile, pcm)
	}
}

func TestReservationCollisionKeepsConcurrentForeignFile(t *testing.T) {
	cfg := localOnDemandRecorder(t, "collision")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	originalReserve := reserveRecordingFile
	var foreign string
	// Simulate a file appearing immediately before the atomic create, not before
	// a separate existence check. The recorder must retry rather than adopt it.
	first := true
	reserveRecordingFile = func(path string) (*os.File, error) {
		if first {
			first = false
			foreign = path
			if err := os.WriteFile(path, []byte("foreign audio"), 0o600); err != nil { //nolint:gosec // Test temp recording.
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
	assertRecordedCapture(t, foreign, []byte("foreign audio"))
}

func TestStopDuringReservationRemovesUnstartedOutput(t *testing.T) {
	cfg := localOnDemandRecorder(t, "stop-reservation")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
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
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	r.state, r.startGen = types.ProcessStarting, 1
	originalReserve := reserveRecordingFile
	reserveRecordingFile = func(path string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
	}
	t.Cleanup(func() { reserveRecordingFile = originalReserve; _ = r.Stop() })
	r.startAsync(1)
	if status := r.Status(); status.State != types.ProcessError || !strings.Contains(status.Error, os.ErrPermission.Error()) || r.result != nil {
		t.Fatalf("reservation failure was ignored: %+v", status)
	}
}

func TestDiscardReservationLeavesReplacementUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserved.ts")
	file, err := reserveRecordingFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output := &recordingOutput{file: file}
	if err := os.Remove(path); err != nil {
		output.close()
		if runtime.GOOS == "windows" {
			t.Skipf("Windows protects the open reservation from replacement: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		output.close()
		t.Fatal(err)
	}
	output.discard()
	assertRecordedCapture(t, path, []byte("replacement"))
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reservation descriptor not closed: %v", err)
	}
}

func TestFailedEncoderStartReleasesReservation(t *testing.T) {
	cfg := localOnDemandRecorder(t, "failed-start")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: filepath.Join(t.TempDir(), "missing-ffmpeg")})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessError)
	// The error is published before the failed-start cleanup finishes.
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir(cfg.LocalPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed startup left reserved files: %v", entries)
		}
		time.Sleep(time.Millisecond)
	}
}
