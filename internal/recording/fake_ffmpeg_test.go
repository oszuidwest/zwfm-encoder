package recording

import (
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
	fakeFFmpegFinalizeEnv = "ZWFM_RECORDING_FAKE_FFMPEG_FINALIZE"
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
// recordings hold the exact PCM written. Like the real FFmpeg with -n it never
// overwrites a file, and it refuses to run without -n. Unlike the real FFmpeg,
// which creates its file at start, the fake writes it only when it exits, so
// its existence shows the recording was finalized.
func runFakeFFmpeg(args []string) int {
	if !slices.Contains(args, "-n") {
		return 2
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1
	}
	finalize := fakeFFmpegFinalize
	if d, err := time.ParseDuration(os.Getenv(fakeFFmpegFinalizeEnv)); err == nil {
		finalize = d
	}
	time.Sleep(finalize)

	output := args[len(args)-1]
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path comes from the recorder under test.
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
	return 0
}
