//go:build windows

package audio

import (
	"regexp"
	"strings"
)

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "ffmpeg",
		DefaultDevice: "", // Auto-detect, no safe default on Windows
		UsesFFmpeg:    true,
		BuildArgs:     buildWindowsArgs,
	}
}

func buildWindowsArgs(device string) []string {
	return buildFFmpegCaptureArgs("dshow", device)
}

func (cfg *CaptureConfig) ListDevices() []Device {
	return parseDeviceList(DeviceListConfig{
		Command: []string{"ffmpeg", "-hide_banner", "-f", "dshow", "-list_devices", "true", "-i", "dummy"},
		// No section markers - FFmpeg versions vary in output format.
		// Some show "DirectShow audio devices" header, others don't.
		// Instead, we filter by lines ending with "(audio)".
		AudioStartMarker: "",
		AudioStopMarker:  "",
		// Match lines like: [dshow @ addr] "Device Name" (audio)
		DevicePattern: regexp.MustCompile(`\[dshow[^\]]*\]\s*"([^"]+)"\s*\(audio\)`),
		ParseDevice: func(matches []string) *Device {
			if len(matches) < 2 {
				return nil
			}
			name := strings.TrimSpace(matches[1])
			return &Device{
				ID:   "audio=" + name,
				Name: name,
			}
		},
		FallbackDevices: nil,
	})
}
