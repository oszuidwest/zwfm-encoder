package util

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ResolveFFmpegPath returns the path to the FFmpeg binary, or empty string if not found.
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

// FFmpegSupportsProtocol reports whether ffmpeg -protocols lists protocol exactly.
func FFmpegSupportsProtocol(ffmpegPath, protocol string) bool {
	if ffmpegPath == "" || protocol == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output() //nolint:gosec // ffmpegPath is resolved config/PATH.
	if err != nil {
		return false
	}
	return FFmpegProtocolListContains(string(out), protocol)
}

// FFmpegProtocolListContains reports whether a protocol list contains protocol as a full token.
func FFmpegProtocolListContains(output, protocol string) bool {
	for _, field := range strings.Fields(output) {
		if field == protocol {
			return true
		}
	}
	return false
}
