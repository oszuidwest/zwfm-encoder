package recording

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// neverRotate replaces the hourly timer so a test rotates by hand and a real
// hour boundary cannot add a rotation. Tests using it must not be parallel.
func neverRotate(t *testing.T) {
	afterFunc = func(time.Duration, func()) *time.Timer { return time.NewTimer(math.MaxInt64) }
	t.Cleanup(func() { afterFunc = time.AfterFunc })
}

func startHourlyTestRecorder(t *testing.T, id string) *GenericRecorder {
	t.Helper()
	cfg := localOnDemandRecorder(t, id)
	cfg.RecordingMode = types.RecordingHourly
	r := NewGenericRecorder(GenericRecorderConfig{
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
				r.rotateFile()
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
	// Named after the wall clock at the rotation, rounded to the minute.
	if got, ok := util.FilenameTime(filepath.Base(secondFile)); !ok ||
		(!got.Equal(rotatedAt.Round(time.Minute)) && !got.Equal(time.Now().Round(time.Minute))) {
		t.Fatalf("rotated file = %s, want it named after %v", filepath.Base(secondFile), rotatedAt.Round(time.Minute))
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
}

// TestStopWaitsForRotatedFile stops the recorder while a rotation is still
// finalizing the previous file: Stop must not return before that file is done.
func TestStopWaitsForRotatedFile(t *testing.T) {
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "500ms")
	r := startHourlyTestRecorder(t, "retiring")
	r.mu.RLock()
	firstFile := r.currentFile
	r.mu.RUnlock()

	// The next file finalizes at once, so only the rotation keeps Stop busy.
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	go r.rotateFile()
	deadline := time.Now().Add(5 * time.Second)
	for r.IsCurrentFile(firstFile) {
		if time.Now().After(deadline) {
			t.Fatal("rotation did not hand over to the next file")
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(firstFile); err != nil {
		t.Fatalf("Stop returned before the rotated file was finalized: %v", err)
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
