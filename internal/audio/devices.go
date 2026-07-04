package audio

import (
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// devicesCacheTTL bounds how often a device listing execs a subprocess: the
// list is fetched by /api/config on every page load and virtually never
// changes on the target hardware.
const devicesCacheTTL = 30 * time.Second

var (
	devicesMu       sync.Mutex
	devicesCached   []Device
	devicesCachedAt time.Time
)

// Devices returns available audio input devices for the current platform,
// served from a short-lived cache so routine config fetches do not fork a
// device-listing subprocess per request.
func Devices() []Device {
	devicesMu.Lock()
	defer devicesMu.Unlock()
	if devicesCached != nil && time.Since(devicesCachedAt) < devicesCacheTTL {
		return devicesCached
	}
	return refreshDevicesLocked()
}

// RefreshDevices bypasses the cache and refills it. Used by the explicit
// device-refresh endpoint so a just-connected device shows up immediately.
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

	//nolint:gosec // Command is from internal platform config, not user input
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	util.HideConsole(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil && len(output) == 0 {
		slog.Error("failed to list audio devices", "error", err)
		return cfg.FallbackDevices
	}

	var devices []Device
	inAudioSection := cfg.AudioStartMarker == "" // If no marker, always in section

	for line := range strings.SplitSeq(string(output), "\n") {
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
