package recording

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestRotationDiscardsReplacedTimer(t *testing.T) {
	for _, mode := range []string{"reschedule", "restart"} {
		t.Run(mode, func(t *testing.T) {
			ManualTimers(t)
			r := startHourlyTestRecorder(t, "stale-timer")
			if mode == "restart" {
				r.Stop()
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				waitForRecorderState(t, r, types.ProcessRunning)
			} else {
				r.inspect(func(*run) { r.scheduleRotation(time.Now()) })
			}
			current := r.currentFile()
			capturedTimers()[0].callback() // Models a callback already queued before Timer.Stop.
			r.settle()
			if r.currentFile() != current || len(capturedTimers()) != 2 {
				t.Fatal("stale callback rotated the current recording or armed another timer")
			}
		})
	}
}

func TestRecorderRejectsStartDuringStop(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "500ms")
	r := startHourlyTestRecorder(t, "stop-start")
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	waitForRecorderState(t, r, types.ProcessStopping)
	err := r.Start()
	if !errors.Is(err, ErrRecorderStopping) {
		t.Errorf("Start during Stop = %v, want ErrRecorderStopping", err)
	}
	if err != nil && r.Status().State != types.ProcessStopping {
		t.Error("rejected Start changed the stopping state")
	}
	<-done
}

// neverRotate replaces the hourly timer so a test rotates by hand and a real
// hour boundary cannot add a rotation. Tests using it must not be parallel.
func neverRotate(t *testing.T) {
	ManualTimers(t)
}

// rotateTestRecorder rotates r as its hourly timer would and waits until the
// next file runs and the previous one is finalized.
func rotateTestRecorder(r *GenericRecorder) {
	r.inspect(func(*run) { r.rotate() })
	r.settle()
}

func startHourlyTestRecorder(t *testing.T, id string) *GenericRecorder {
	t.Helper()
	cfg := localOnDemandRecorder(t, id)
	cfg.RecordingMode = types.RecordingHourly
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	startCaptureTestRecorder(t, r)
	return r
}

func TestHourlyRotationLosesNoAudio(t *testing.T) {
	neverRotate(t)
	r := startHourlyTestRecorder(t, "gapless")

	firstFile := r.currentFile()

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
	secondFile := r.currentFile()
	r.Stop()
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
	// A rotation at a boundary schedules the next one a full hour later, also
	// when it ran a moment early and its time was rounded up to the boundary.
	boundary := time.Now().Truncate(time.Hour).Add(time.Hour)
	if got, want := nextHour(boundary), boundary.Add(time.Hour); !got.Equal(want) {
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
	firstFile := r.currentFile()

	// The next file finalizes at once, so only the rotation keeps Stop busy.
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	go rotateTestRecorder(r)
	waitForHandOver(t, r, firstFile)
	if !r.outbox.hasPath(firstFile) {
		t.Fatal("the file being finalized is not protected from retention cleanup")
	}
	r.Stop()
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
	firstFile := r.currentFile()
	r.ffmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")

	chunk := make([]byte, audio.BytesPerSecond/10)
	r.WriteAudio(chunk)
	rotateTestRecorder(r)
	if status := r.Status(); status.State != types.ProcessRunning || status.Error == "" || r.currentFile() != firstFile {
		t.Fatalf("after failed rotation: status %+v, current file %s; want running with an error in %s",
			status, r.currentFile(), filepath.Base(firstFile))
	}
	r.WriteAudio(chunk)

	r.ffmpegPath = os.Args[0]
	rotateTestRecorder(r)
	if status := r.Status(); status.Error != "" || r.currentFile() == firstFile {
		t.Fatalf("after a successful rotation: status %+v, current file %s; want the error cleared",
			status, r.currentFile())
	}
	r.Stop()
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
	firstFile := r.currentFile()

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
	var current string
	if !WaitFor(5*time.Second, func() bool {
		current = r.currentFile()
		return current != "" && current != from
	}) {
		t.Fatal("rotation did not hand over to the next file")
	}
	return current
}

func waitForRecorderState(t *testing.T, r *GenericRecorder, want types.ProcessState) {
	t.Helper()
	if !WaitFor(5*time.Second, func() bool { return r.Status().State == want }) {
		t.Fatalf("recorder state = %q, want %q", r.Status().State, want)
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
