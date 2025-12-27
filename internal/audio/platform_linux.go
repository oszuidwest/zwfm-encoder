//go:build linux

package audio

import (
	"regexp"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func getPlatformConfig() CaptureConfig {
	return CaptureConfig{
		Command:       "arecord",
		InputFormat:   "",
		DevicePrefix:  "",
		DefaultDevice: "default:CARD=sndrpihifiberry",
		BuildArgs:     buildLinuxArgs,
	}
}

func buildLinuxArgs(device string) []string {
	return []string{
		"-D", device,
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "2",
		"-t", "raw",
		"-q",
		"-",
	}
}

func (cfg CaptureConfig) ListDevices() []types.AudioDevice {
	return parseDeviceList(&DeviceListConfig{
		Command:          []string{"arecord", "-l"},
		AudioStartMarker: "", // No marker, parse all lines
		DevicePattern:    regexp.MustCompile(`card\s+(\d+):\s+(\w+)\s+\[([^\]]+)\]`),
		ParseDevice: func(matches []string) *types.AudioDevice {
			if len(matches) < 4 {
				return nil
			}
			return &types.AudioDevice{
				ID:   "default:CARD=" + matches[2],
				Name: matches[3],
			}
		},
		FallbackDevices: []types.AudioDevice{
			{ID: "default:CARD=sndrpihifiberry", Name: "HiFiBerry (default)"},
		},
	})
}
