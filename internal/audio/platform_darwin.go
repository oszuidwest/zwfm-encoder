//go:build darwin

package audio

import (
	"regexp"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "ffmpeg",
		InputFormat:   "avfoundation",
		DevicePrefix:  ":",
		DefaultDevice: ":0",
		BuildArgs:     buildDarwinArgs,
	}
}

func buildDarwinArgs(device string) []string {
	return []string{
		"-f", "avfoundation",
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
