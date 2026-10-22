package recording

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestRotationDiscardsReplacedTimer(t *testing.T) {
	for _, mode := range []string{"reschedule", "restart", "restart on demand"} {
		t.Run(mode, func(t *testing.T) {
			var callbacks []func()
			var timers []*time.Timer
			afterFunc = func(_ time.Duration, callback func()) *time.Timer {
				callbacks = append(callbacks, callback)
				timer := time.NewTimer(math.MaxInt64)
				timers = append(timers, timer)
				return timer
			}
			t.Cleanup(func() { afterFunc = time.AfterFunc })
			r := startHourlyTestRecorder(t, "stale-timer")
			wantCallbacks := 2
			if mode != "reschedule" {
				if err := r.Stop(); err != nil {
					t.Fatal(err)
				}
				if mode == "restart on demand" {
					// An on-demand restart arms no rotation timer of its own.
					r.mu.Lock()
					r.config.RecordingMode = types.RecordingOnDemand
					r.mu.Unlock()
					wantCallbacks = 1
				}
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				waitForRecorderState(t, r, types.ProcessRunning)
			} else {
				r.mu.Lock()
				r.scheduleRotationLocked(time.Now())
				r.mu.Unlock()
			}
			if timers[0].Stop() {
				t.Error("replaced rotation timer was still armed")
			}
			r.mu.RLock()
			current, timer := r.currentFile, r.rotationTimer
			r.mu.RUnlock()
			callbacks[0]() // Models a callback already queued before Timer.Stop.
			r.mu.RLock()
			defer r.mu.RUnlock()
			if r.currentFile != current || r.rotationTimer != timer || len(callbacks) != wantCallbacks {
				t.Fatal("stale callback rotated the current recording or armed another timer")
			}
		})
	}
}

func TestRecorderRejectsStartDuringStop(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "500ms")
	r := startHourlyTestRecorder(t, "stop-start")
	done := make(chan error, 1)
	go func() { done <- r.Stop() }()
	waitForRecorderState(t, r, types.ProcessStopping)
	err := r.Start()
	if !errors.Is(err, ErrRecorderStopping) {
		t.Errorf("Start during Stop = %v, want ErrRecorderStopping", err)
	}
	if err != nil && r.Status().State != types.ProcessStopping {
		t.Error("rejected Start changed the stopping state")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriteErrorDuringStopKeepsStartRejected(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "write-error-stop")
	// Models a write failure that takes r.mu after Stop set Stopping but
	// before Stop claimed the encoder.
	r.mu.Lock()
	r.state = types.ProcessStopping
	result, audioCh, writerDone := r.result, r.audioCh, r.writerDone
	r.mu.Unlock()
	r.handleWriteError(result, errors.New("broken pipe"))
	close(audioCh) // The failing audioWriter returns after handleWriteError.
	if err := r.Start(); !errors.Is(err, ErrRecorderStopping) {
		t.Errorf("Start after a write error during Stop = %v, want ErrRecorderStopping", err)
	}
	if status := r.Status(); status.State != types.ProcessStopping || status.Error != "broken pipe" {
		t.Errorf("status = %+v, want stopping with the write error", status)
	}
	<-writerDone
	_ = result.Wait() // Write-error cleanup owns the detached FFmpeg.
	r.mu.Lock()
	r.state = types.ProcessError // Let the cleanup Stop finish the modeled teardown.
	r.mu.Unlock()
}

// neverRotate replaces the hourly timer so a test rotates by hand and a real
// hour boundary cannot add a rotation. Tests using it must not be parallel.
func neverRotate(t *testing.T) {
	afterFunc = func(time.Duration, func()) *time.Timer { return time.NewTimer(math.MaxInt64) }
	t.Cleanup(func() { afterFunc = time.AfterFunc })
}

func rotateTestRecorder(r *GenericRecorder) {
	r.mu.RLock()
	generation := r.rotationGeneration
	r.mu.RUnlock()
	r.rotateFile(generation)
}

func startHourlyTestRecorder(t *testing.T, id string) *GenericRecorder {
	t.Helper()
	cfg := localOnDemandRecorder(t, id)
	cfg.RecordingMode = types.RecordingHourly
	r := newTestRecorder(t, GenericRecorderConfig{
		Recorder:   cfg,
		FFmpegPath: os.Args[0],
		SpoolDir:   t.TempDir(),
	})
	if err := r.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })
	waitForRecorderState(t, r, types.ProcessRunning)
	return r
}

func TestHourlyRotationLosesNoAudio(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "gapless")

	r.mu.RLock()
	firstFile := r.currentFile
	r.mu.RUnlock()

	// Every sample carries its own index, so a lost, repeated or reordered
	// chunk changes the recorded audio.
	const chunks = 40
	chunkSize := audio.BytesPerSecond / 10
	input := make([]byte, chunks*chunkSize)
	for i := range len(input) / 2 {
		binary.LittleEndian.PutUint16(input[2*i:], uint16(i))
	}

	// Rotate while chunks keep flowing from this goroutine.
	rotated := make(chan struct{})
	var rotatedAt time.Time
	for i := range chunks {
		switch i {
		case chunks / 4:
			rotatedAt = time.Now()
			go func() {
				rotateTestRecorder(r)
				close(rotated)
			}()
		case chunks - 1:
			<-rotated // At least the last chunk must reach the next file.
		}
		r.WriteAudio(input[i*chunkSize : (i+1)*chunkSize])
		time.Sleep(5 * time.Millisecond) // Gives the rotation chunks to race with.
	}
	r.mu.RLock()
	secondFile := r.currentFile
	r.mu.RUnlock()
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if drops := r.Status().AudioDrops; drops != 0 {
		t.Fatalf("AudioDrops = %d, want 0", drops)
	}

	var recorded []byte
	for _, file := range []string{firstFile, secondFile} {
		data, err := os.ReadFile(file) //nolint:gosec // G304: test reads its own temp recordings.
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(file), err)
		}
		if len(data) == 0 {
			t.Fatalf("%s holds no audio; the rotation did not split the recording", filepath.Base(file))
		}
		recorded = append(recorded, data...)
	}
	if !bytes.Equal(recorded, input) {
		t.Fatalf("recorded %d bytes across the rotation, want the %d input bytes unchanged (first difference at byte %d)",
			len(recorded), len(input), firstDifference(recorded, input))
	}
	// Named after the wall clock at the rotation, rounded to the minute.
	if got, ok := util.FilenameTime(filepath.Base(secondFile)); !ok ||
		(!got.Equal(rotatedAt.Round(time.Minute)) && !got.Equal(time.Now().Round(time.Minute))) {
		t.Fatalf("rotated file = %s, want it named after %v", filepath.Base(secondFile), rotatedAt.Round(time.Minute))
	}
}

func TestScheduleRotationTargetsNextHour(t *testing.T) {
	var delay time.Duration
	afterFunc = func(d time.Duration, _ func()) *time.Timer {
		delay = d
		return time.NewTimer(math.MaxInt64)
	}
	t.Cleanup(func() { afterFunc = time.AfterFunc })

	// A rotation at a boundary schedules the next one a full hour later, also
	// when it ran a moment early and its time was rounded up to the boundary.
	boundary := time.Now().Truncate(time.Hour).Add(time.Hour)
	(&GenericRecorder{}).scheduleRotationLocked(boundary)
	want := boundary.Add(time.Hour)
	if got := time.Now().Add(delay); got.Sub(want).Abs() > time.Second {
		t.Fatalf("next rotation at %v, want %v", got, want)
	}
}

// TestStopWaitsForRotatedFile stops the recorder while a rotation is still
// finalizing the previous file: retention cleanup must leave that file alone,
// and Stop must not return before it is done.
func TestStopWaitsForRotatedFile(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "500ms")
	r := startHourlyTestRecorder(t, "retiring")
	r.WriteAudio([]byte{1, 2, 3, 4})
	r.mu.RLock()
	firstFile := r.currentFile
	r.mu.RUnlock()

	// The next file finalizes at once, so only the rotation keeps Stop busy.
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	go rotateTestRecorder(r)
	waitForHandOver(t, r, firstFile)
	if !r.outbox.hasPath(firstFile) {
		t.Fatal("the file being finalized is not protected from retention cleanup")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(firstFile); err != nil {
		t.Fatalf("Stop returned before the rotated file was finalized: %v", err)
	}
	if r.outbox.hasPath(firstFile) {
		t.Fatal("the finalized file is still protected from retention cleanup")
	}
}

// TestFailedRotationKeepsRecording covers a next file that cannot start: the
// recorder keeps writing into the current file instead of stopping, and the
// next successful rotation clears the error.
func TestFailedRotationKeepsRecording(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "fallback")
	r.mu.Lock()
	firstFile := r.currentFile
	r.ffmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	r.mu.Unlock()

	chunk := make([]byte, audio.BytesPerSecond/10)
	r.WriteAudio(chunk)
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || status.Error == "" || !r.IsCurrentFile(firstFile) {
		t.Fatalf("after failed rotation: status %+v, current file kept %t; want running with an error in %s",
			status, r.IsCurrentFile(firstFile), filepath.Base(firstFile))
	}
	r.WriteAudio(chunk)

	r.mu.Lock()
	r.ffmpegPath = os.Args[0]
	r.mu.Unlock()
	rotateTestRecorder(r)
	if status := r.Status(); status.Error != "" || r.IsCurrentFile(firstFile) {
		t.Fatalf("after a successful rotation: status %+v, still in the first file %t; want the error cleared",
			status, r.IsCurrentFile(firstFile))
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	data, err := os.ReadFile(firstFile) //nolint:gosec // G304: test reads its own temp recording.
	if err != nil || len(data) != 2*len(chunk) {
		t.Fatalf("current file holds %d bytes, %v; want both chunks (%d bytes)", len(data), err, 2*len(chunk))
	}
}

// TestOverlappingRotationsProtectEveryRetiringFile rotates again while the
// first rotation is still finalizing: both retiring files stay protected.
func TestOverlappingRotationsProtectEveryRetiringFile(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "1s")
	r := startHourlyTestRecorder(t, "overlap")
	r.mu.RLock()
	firstFile := r.currentFile
	r.mu.RUnlock()

	go rotateTestRecorder(r)
	secondFile := waitForHandOver(t, r, firstFile)
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	go rotateTestRecorder(r)
	waitForHandOver(t, r, secondFile)
	if !r.outbox.hasPath(firstFile) || !r.outbox.hasPath(secondFile) {
		t.Fatalf("finalizing files protected: first %t, second %t; want both",
			r.outbox.hasPath(firstFile), r.outbox.hasPath(secondFile))
	}
}

// waitForHandOver waits until a rotation has moved r off from and returns the
// new current file.
func waitForHandOver(t *testing.T, r *GenericRecorder, from string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.RLock()
		current := r.currentFile
		r.mu.RUnlock()
		if current != from {
			return current
		}
		if time.Now().After(deadline) {
			t.Fatal("rotation did not hand over to the next file")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForRecorderState(t *testing.T, r *GenericRecorder, want types.ProcessState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.Status().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recorder state = %q, want %q", r.Status().State, want)
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
