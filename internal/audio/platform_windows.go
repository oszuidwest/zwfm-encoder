//go:build windows

package audio

import (
	"regexp"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "ffmpeg",
		InputFormat:   "dshow",
		DevicePrefix:  "audio=",
		DefaultDevice: "", // Auto-detect, no safe default on Windows
		BuildArgs:     buildWindowsArgs,
	}
}

func buildWindowsArgs(device string) []string {
	return []string{
		"-f", "dshow",
		"-i", device,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "warning",
		"-vn",
		"-f", "s16le",
		"-ac", "2",
		"-ar", "48000",
		"pipe:1",
	}
}

func (cfg CaptureConfig) ListDevices() []types.AudioDevice {
	return parseDeviceList(&DeviceListConfig{
		Command:          []string{"ffmpeg", "-f", "dshow", "-list_devices", "true", "-i", "dummy"},
		AudioStartMarker: "DirectShow audio devices",
		AudioStopMarker:  "DirectShow video devices",
		DevicePattern:    regexp.MustCompile(`\[dshow[^\]]*\]\s*"([^"]+)"`),
		ParseDevice: func(matches []string) *types.AudioDevice {
			if len(matches) < 2 {
				return nil
			}
			name := strings.TrimSpace(matches[1])
			return &types.AudioDevice{
				ID:   "audio=" + name,
				Name: name,
			}
		},
		FallbackDevices: nil,
	})
}
