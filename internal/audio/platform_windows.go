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

// windowsDevicePattern matches lines like: [dshow @ addr] "Device Name" (audio).
var windowsDevicePattern = regexp.MustCompile(`\[dshow[^\]]*\]\s*"([^"]+)"\s*\(audio\)`)

// Devices lists DirectShow audio inputs without using the device cache.
func (cfg *CaptureConfig) Devices() []Device {
	return parseDeviceList(DeviceListConfig{
		Command: []string{"ffmpeg", "-hide_banner", "-f", "dshow", "-list_devices", "true", "-i", "dummy"},
		// Match "(audio)" lines: FFmpeg's section headers vary by version.
		AudioStartMarker: "",
		AudioStopMarker:  "",
		DevicePattern:    windowsDevicePattern,
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
