package recording

import (
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"
)

const (
	// fakeFFmpegEnv makes the test binary act as FFmpeg; tests pass os.Args[0]
	// as the FFmpeg path.
	fakeFFmpegEnv = "ZWFM_RECORDING_FAKE_FFMPEG"
	// fakeFFmpegFinalizeEnv overrides how long the fake takes to finalize.
	fakeFFmpegFinalizeEnv     = "ZWFM_RECORDING_FAKE_FFMPEG_FINALIZE"
	fakeFFmpegFailFinalizeEnv = "ZWFM_RECORDING_FAKE_FFMPEG_FAIL_FINALIZE"
	fakeFFmpegEmptyHeaderEnv  = "ZWFM_RECORDING_FAKE_FFMPEG_EMPTY_HEADER"
	fakeFFmpegFailWriteEnv    = "ZWFM_RECORDING_FAKE_FFMPEG_FAIL_WRITE"
	// fakeFFmpegFinalize is how long the fake takes to exit after stdin closes,
	// like a real encoder flushing and finalizing its file.
	fakeFFmpegFinalize = 100 * time.Millisecond
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeFFmpegEnv) == "1" {
		os.Exit(runFakeFFmpeg(os.Args[1:]))
	}
	// FFmpeg processes started by the tests inherit these. A race-enabled fake
	// would otherwise sleep 1s at exit (GORACE atexit_sleep_ms).
	if err := os.Setenv(fakeFFmpegEnv, "1"); err != nil {
		panic(err)
	}
	if err := os.Setenv("GORACE", "atexit_sleep_ms=0 "+os.Getenv("GORACE")); err != nil {
		panic(err)
	}
	// Recorder tests must not depend on the host's NTP state.
	clockSynced = func() bool { return true }
	os.Exit(m.Run())
}

// runFakeFFmpeg copies stdin to the output file (the last argument), so
// recordings hold the exact PCM written. It requires -y and an already-reserved
// output. Data is written only on exit, simulating delayed encoder finalization.
func runFakeFFmpeg(args []string) int {
	if !slices.Contains(args, "-y") {
		return 2
	}
	output := args[len(args)-1]
	reader := io.Reader(os.Stdin)
	failWrite := os.Getenv(fakeFFmpegFailWriteEnv)
	if failWrite != "" {
		limit := int64(4096)
		if failWrite == "empty" { // Fails before encoding any audio.
			limit = 0
		}
		reader = io.LimitReader(reader, limit)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return 1
	}
	if len(data) == 0 && os.Getenv(fakeFFmpegEmptyHeaderEnv) == "1" {
		data = []byte("header without audio")
	}
	finalize := fakeFFmpegFinalize
	if d, err := time.ParseDuration(os.Getenv(fakeFFmpegFinalizeEnv)); err == nil {
		finalize = d
	}
	time.Sleep(finalize)

	file, err := os.OpenFile(output, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: path comes from the recorder under test.
	if err != nil {
		return 1
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return 1
	}
	if err := file.Close(); err != nil {
		return 1
	}
	if failWrite != "" {
		_, _ = fmt.Fprintln(os.Stderr, "simulated write failure")
		return 1
	}
	if os.Getenv(fakeFFmpegFailFinalizeEnv) == "1" {
		_, _ = fmt.Fprintln(os.Stderr, "Error writing trailer: No space left on device")
		return 1
	}
	return 0
}
