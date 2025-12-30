//go:build darwin

package audio

import (
	"regexp"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "ffmpeg",
		DefaultDevice: ":0",
		BuildArgs:     buildDarwinArgs,
	}
}

func buildDarwinArgs(device string) []string {
	return buildFFmpegCaptureArgs("avfoundation", device)
}

func (cfg CaptureConfig) ListDevices() []types.AudioDevice {
	return parseDeviceList(DeviceListConfig{
		Command:          []string{"ffmpeg", "-f", "avfoundation", "-list_devices", "true", "-i", ""},
		AudioStartMarker: "AVFoundation audio devices:",
		AudioStopMarker:  "AVFoundation video devices:",
		DevicePattern:    regexp.MustCompile(`\[AVFoundation[^\]]*\]\s*\[(\d+)\]\s*(.+)`),
		ParseDevice: func(matches []string) *types.AudioDevice {
			if len(matches) < 3 {
				return nil
			}
			return &types.AudioDevice{
				ID:   ":" + matches[1],
				Name: matches[2],
			}
		},
		FallbackDevices: nil,
	})
}
