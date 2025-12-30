package audio

import "errors"

// ErrNoAudioDevice is returned when no audio input device is available.
var ErrNoAudioDevice = errors.New("no audio input device found")

// CaptureConfig defines platform-specific audio capture configuration.
type CaptureConfig struct {
	// Command is the executable name (e.g., "arecord", "ffmpeg").
	Command string

	// DefaultDevice is used when no device is configured.
	DefaultDevice string

	// BuildArgs returns the command arguments for audio capture.
	// The device parameter is the audio input device identifier.
	BuildArgs func(device string) []string
}

// BuildCaptureCommand returns the command and arguments for audio capture.
// If device is empty, it attempts to use the default or auto-detect.
func BuildCaptureCommand(device string) (cmd string, args []string, err error) {
	cfg := getPlatformConfig()

	if device == "" {
		device = cfg.DefaultDevice
	}

	// Auto-detect if still empty (Windows has no safe default).
	if device == "" {
		devices := ListDevices()
		if len(devices) == 0 {
			return "", nil, ErrNoAudioDevice
		}
		device = devices[0].ID
	}

	return cfg.Command, cfg.BuildArgs(device), nil
}
