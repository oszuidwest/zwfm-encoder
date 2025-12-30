package audio

import "errors"

// ErrNoAudioDevice is returned when no audio input device is available.
var ErrNoAudioDevice = errors.New("no audio input device found")

// CaptureConfig defines platform-specific audio capture configuration.
type CaptureConfig struct {
	// Command is the executable name (e.g., "arecord", "ffmpeg").
	Command string

	// InputFormat is the FFmpeg input format (e.g., "avfoundation", "dshow").
	// Empty for non-FFmpeg backends like arecord.
	InputFormat string

	// DevicePrefix is prepended to device IDs (e.g., "audio=" for DirectShow).
	DevicePrefix string

	// DefaultDevice is used when no device is configured.
	DefaultDevice string

	// UsesFFmpeg indicates if this platform uses FFmpeg for capture.
	UsesFFmpeg bool

	// BuildArgs returns the command arguments for audio capture.
	// The device parameter is the audio input device identifier.
	BuildArgs func(device string) []string
}

// BuildCaptureCommand returns the command and arguments for audio capture.
// If device is empty, it attempts to use the default or auto-detect.
// The ffmpegPath parameter is used on platforms that use FFmpeg for capture.
func BuildCaptureCommand(device, ffmpegPath string) (cmd string, args []string, err error) {
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

	// Use provided ffmpegPath on platforms that use FFmpeg for capture
	command := cfg.Command
	if cfg.UsesFFmpeg && ffmpegPath != "" {
		command = ffmpegPath
	}

	return command, cfg.BuildArgs(device), nil
}
