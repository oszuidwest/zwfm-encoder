package util

import "os/exec"

// ResolveFFmpegPath returns the path to the FFmpeg binary.
// If customPath is set, it validates the path exists and is executable.
// Otherwise, it searches for "ffmpeg" in the system PATH.
// Returns an empty string if FFmpeg is not found.
func ResolveFFmpegPath(customPath string) string {
	if customPath != "" {
		if _, err := exec.LookPath(customPath); err == nil {
			return customPath
		}
		return ""
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	return path
}
