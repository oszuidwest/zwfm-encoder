package util

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const ffmpegProtocolProbeTimeout = 5 * time.Second

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

// ProbeFFmpegProtocol checks whether "ffmpeg -protocols" lists protocol exactly.
// Probe errors are returned separately from a clean "protocol not listed"
// result so callers do not mislabel transient probe failures as build support.
func ProbeFFmpegProtocol(ffmpegPath, protocol string) (bool, error) {
	if ffmpegPath == "" || protocol == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegProtocolProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output() //nolint:gosec // ffmpegPath is resolved config/PATH.
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, fmt.Errorf("probe ffmpeg protocols timed out after %s: %w", ffmpegProtocolProbeTimeout, ctx.Err())
		}
		return false, fmt.Errorf("probe ffmpeg protocols: %w", err)
	}
	return FFmpegProtocolListContains(string(out), protocol), nil
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
