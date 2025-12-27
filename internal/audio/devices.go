package audio

import (
	"log/slog"
	"os/exec"
	"regexp"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// ListDevices returns available audio input devices for the current platform.
func ListDevices() []types.AudioDevice {
	cfg := getPlatformConfig()
	return cfg.ListDevices()
}

// DeviceListConfig defines how to list audio devices for a platform.
type DeviceListConfig struct {
	// Command and args to list devices.
	Command []string

	// AudioStartMarker indicates the start of audio devices section.
	AudioStartMarker string

	// AudioStopMarker indicates the end of audio devices section (optional).
	AudioStopMarker string

	// DevicePattern is the regex to extract device info.
	// Group 1: device ID or index, Group 2: device name (optional).
	DevicePattern *regexp.Regexp

	// ParseDevice converts regex matches to a Device.
	ParseDevice func(matches []string) *types.AudioDevice

	// FallbackDevices are returned if detection fails.
	FallbackDevices []types.AudioDevice
}

// parseDeviceList is a shared helper for parsing device list output.
//
//nolint:gocritic // hugeParam: 96 bytes is acceptable, no performance impact
func parseDeviceList(cfg DeviceListConfig) []types.AudioDevice {
	if len(cfg.Command) == 0 {
		return cfg.FallbackDevices
	}

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil && len(output) == 0 {
		slog.Error("failed to list audio devices", "error", err)
		return cfg.FallbackDevices
	}

	var devices []types.AudioDevice
	lines := strings.Split(string(output), "\n")
	inAudioSection := cfg.AudioStartMarker == "" // If no marker, always in section

	for _, line := range lines {
		// Check for section markers.
		if cfg.AudioStartMarker != "" && strings.Contains(line, cfg.AudioStartMarker) {
			inAudioSection = true
			continue
		}
		if cfg.AudioStopMarker != "" && strings.Contains(line, cfg.AudioStopMarker) {
			inAudioSection = false
			continue
		}

		if !inAudioSection {
			continue
		}

		// Skip alternative name lines (Windows DirectShow).
		if strings.Contains(line, "Alternative name") {
			continue
		}

		if cfg.DevicePattern == nil {
			continue
		}

		matches := cfg.DevicePattern.FindStringSubmatch(line)
		if len(matches) > 0 && cfg.ParseDevice != nil {
			if dev := cfg.ParseDevice(matches); dev != nil {
				devices = append(devices, *dev)
			}
		}
	}

	if len(devices) == 0 {
		return cfg.FallbackDevices
	}

	return devices
}
