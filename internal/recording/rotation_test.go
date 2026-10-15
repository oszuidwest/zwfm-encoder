package recording

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// fakeFFmpegEnv makes the test binary act as FFmpeg: it copies stdin to the
// output file (the last argument), so recordings hold the exact PCM written.
const fakeFFmpegEnv = "ZWFM_RECORDING_FAKE_FFMPEG"

// fakeFFmpegFinalize is how long the fake FFmpeg takes to exit after stdin
// closes, like a real encoder flushing and finalizing its file.
const fakeFFmpegFinalize = 100 * time.Millisecond

func TestMain(m *testing.M) {
	if os.Getenv(fakeFFmpegEnv) == "1" {
		os.Exit(runFakeFFmpeg(os.Args[len(os.Args)-1]))
	}
	os.Exit(m.Run())
}

func runFakeFFmpeg(output string) int {
	file, err := os.Create(output) //nolint:gosec // G304: path comes from the recorder under test.
	if err != nil {
		return 1
	}
	if _, err := io.Copy(file, os.Stdin); err != nil {
		_ = file.Close()
		return 1
	}
	time.Sleep(fakeFFmpegFinalize)
	if err := file.Close(); err != nil {
		return 1
	}
	return 0
}

func TestRotationIntoRepeatedDSTHourGetsSuffix(t *testing.T) {
	t.Parallel()
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("time zone data unavailable: %v", err)
	}
	cfg := &types.Recorder{Name: "Archive", Codec: types.CodecMP3}
	never := func(string) bool { return false }
	// 00:00 and 01:00 UTC on 25 October 2026 are both 02:00 local time.
	summer := nextRecordingFilename(cfg, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC).In(amsterdam), never, nil)
	used := map[string]struct{}{summer: {}}
	winter := nextRecordingFilename(cfg, time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC).In(amsterdam), never, used)
	if summer != "Archive-2026-10-25-02-00.mp3" || winter != "Archive-2026-10-25-02-00-2.mp3" {
		t.Fatalf("repeated hour files = %q, %q; want the second one suffixed", summer, winter)
	}
}

func TestHourlyRotationLosesNoAudio(t *testing.T) {
	t.Setenv(fakeFFmpegEnv, "1")
	// The test rotates by hand; a real hour boundary must not add a rotation.
	// Not parallel, so no other test schedules rotations meanwhile.
	afterFunc = func(time.Duration, func()) *time.Timer { return time.NewTimer(math.MaxInt64) }
	t.Cleanup(func() { afterFunc = time.AfterFunc })

	dir := t.TempDir()
	r := NewGenericRecorder(GenericRecorderConfig{
		Recorder: &types.Recorder{
			ID:            "gapless",
			Name:          "gapless",
			Codec:         types.CodecPCM,
			RecordingMode: types.RecordingHourly,
			StorageMode:   types.StorageLocal,
			LocalPath:     dir,
		},
		FFmpegPath: os.Args[0],
		SpoolDir:   t.TempDir(),
	})
	if err := r.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })
	waitForRecorderState(t, r, types.ProcessRunning)

	r.mu.RLock()
	firstFile := r.currentFile
	boundary := r.nextRotation
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
	rotate := make(chan struct{})
	rotated := make(chan struct{})
	go func() {
		<-rotate
		r.rotateFile()
		close(rotated)
	}()
	for i := range chunks {
		switch i {
		case chunks / 4:
			close(rotate)
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
	if want := "gapless-" + boundary.Format("2006-01-02-15-04") + ".ts"; filepath.Base(secondFile) != want {
		t.Fatalf("rotated file = %s, want %s named after the scheduled boundary", filepath.Base(secondFile), want)
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
