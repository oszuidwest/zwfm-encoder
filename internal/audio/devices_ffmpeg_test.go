//go:build darwin || windows

package audio

import (
	"fmt"
	"os"
	"testing"
)

const fakeFFmpegEnv = "AUDIO_FAKE_FFMPEG"

// TestMain lets the test binary act as FFmpeg, so the device list can run a
// configured binary that is not on PATH.
func TestMain(m *testing.M) {
	if os.Getenv(fakeFFmpegEnv) == "1" {
		// One device for each platform's parser; FFmpeg lists on stderr.
		_, _ = fmt.Fprint(os.Stderr, "[AVFoundation indev @ 0x1] AVFoundation audio devices:\n"+
			"[AVFoundation indev @ 0x1] [0] Fake Input\n"+
			"[dshow @ 0x1] \"Fake Input\" (audio)\n")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestRefreshDevicesUsesConfiguredFFmpeg(t *testing.T) {
	// Keep this test serial: the FFmpeg path and device cache are package-wide.
	t.Setenv(fakeFFmpegEnv, "1")
	SetFFmpegPath(os.Args[0])
	t.Cleanup(func() {
		SetFFmpegPath("")
		devicesMu.Lock()
		devicesCached = nil
		devicesMu.Unlock()
	})

	devices := RefreshDevices()
	if len(devices) != 1 || devices[0].Name != "Fake Input" {
		t.Fatalf("RefreshDevices() = %+v, want only Fake Input from the configured FFmpeg", devices)
	}
}
