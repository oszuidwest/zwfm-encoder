//go:build darwin

package audio

import "regexp"

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "ffmpeg",
		DefaultDevice: ":0",
		UsesFFmpeg:    true,
		BuildArgs:     buildDarwinArgs,
	}
}

func buildDarwinArgs(device string) []string {
	return buildFFmpegCaptureArgs("avfoundation", device)
}

func (cfg *CaptureConfig) Devices() []Device {
	return parseDeviceList(DeviceListConfig{
		Command:          []string{"ffmpeg", "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", ""},
		AudioStartMarker: "AVFoundation audio devices:",
		AudioStopMarker:  "AVFoundation video devices:",
		DevicePattern:    regexp.MustCompile(`\[AVFoundation[^\]]*\]\s*\[(\d+)\]\s*(.+)`),
		ParseDevice: func(matches []string) *Device {
			if len(matches) < 3 {
				return nil
			}
			return &Device{
				ID:   ":" + matches[1],
				Name: matches[2],
			}
		},
		FallbackDevices: nil,
	})
}
