package audio

import "errors"

// Audio format constants for PCM capture and encoding.
const (
	// SampleRate is the audio sample rate in Hz.
	SampleRate = 48000
	// Channels is the number of audio channels (stereo).
	Channels = 2
)

// ErrNoAudioDevice is returned when no audio input device is available.
var ErrNoAudioDevice = errors.New("no audio input device found")

// CaptureConfig defines platform-specific audio capture configuration.
type CaptureConfig struct {
	// Command is the executable name (e.g., "arecord", "ffmpeg").
	Command string

	// DefaultDevice is used when no device is configured.
	DefaultDevice string

	// UsesFFmpeg indicates if this platform uses FFmpeg for capture.
	UsesFFmpeg bool

	// BuildArgs returns the command arguments for audio capture.
	// The device parameter is the audio input device identifier.
	BuildArgs func(device string) []string
}

// BuildCaptureCommand returns the command and arguments for audio capture.
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
