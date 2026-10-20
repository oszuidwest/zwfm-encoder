package audio

import (
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// devicesCacheTTL avoids a device-listing subprocess on every config fetch.
const devicesCacheTTL = 30 * time.Second

var (
	devicesMu       sync.Mutex
	devicesCached   []Device
	devicesCachedAt time.Time
)

// Devices returns available audio inputs, cached for devicesCacheTTL.
func Devices() []Device {
	devicesMu.Lock()
	defer devicesMu.Unlock()
	if devicesCached != nil && time.Since(devicesCachedAt) < devicesCacheTTL {
		return devicesCached
	}
	return refreshDevicesLocked()
}

// RefreshDevices bypasses and refills the device cache.
func RefreshDevices() []Device {
	devicesMu.Lock()
	defer devicesMu.Unlock()
	return refreshDevicesLocked()
}

func refreshDevicesLocked() []Device {
	cfg := getPlatformConfig()
	devicesCached = cfg.Devices()
	devicesCachedAt = time.Now()
	return devicesCached
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
	DevicePattern *regexp.Regexp

	// ParseDevice converts regex matches to a Device.
	ParseDevice func(matches []string) *Device

	// FallbackDevices are returned if detection fails.
	FallbackDevices []Device
}

// parseDeviceList parses command output to extract audio device information.
//
//nolint:gocritic // hugeParam: 96 bytes is acceptable, no performance impact
func parseDeviceList(cfg DeviceListConfig) []Device {
	if len(cfg.Command) == 0 {
		return cfg.FallbackDevices
	}

	cmd := util.Command(cfg.Command[0], cfg.Command[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil && len(output) == 0 {
		slog.Error("failed to list audio devices", "error", err)
		return cfg.FallbackDevices
	}

	var devices []Device
	inAudioSection := cfg.AudioStartMarker == "" // If no marker, always in section

	for line := range strings.SplitSeq(string(output), "\n") {
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
