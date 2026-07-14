package util

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	ffmpegProtocolProbeAttempts = 3
	ffmpegProtocolProbeTimeout  = 5 * time.Second
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

// probeFFmpegProtocols runs "ffmpeg -protocols"; a package variable so tests can stub the exec.
var probeFFmpegProtocols = func(ffmpegPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegProtocolProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output() //nolint:gosec // ffmpegPath is resolved config/PATH.
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Output reports "signal: killed" on timeout; surface the deadline error instead.
		return nil, ctx.Err()
	}
	return out, err
}

// ProbeFFmpegProtocol checks whether "ffmpeg -protocols" lists protocol exactly,
// retrying probe timeouts. Probe errors are returned separately from a clean
// "protocol not listed" result so callers do not mislabel transient probe
// failures as build support.
func ProbeFFmpegProtocol(ffmpegPath, protocol string) (bool, error) {
	if ffmpegPath == "" || protocol == "" {
		return false, nil
	}
	for range ffmpegProtocolProbeAttempts {
		out, err := probeFFmpegProtocols(ffmpegPath)
		if err == nil {
			return FFmpegProtocolListContains(string(out), protocol), nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("probe ffmpeg protocols: %w", err)
		}
	}
	return false, fmt.Errorf("probe ffmpeg protocols timed out after %d attempts of %s: %w",
		ffmpegProtocolProbeAttempts, ffmpegProtocolProbeTimeout, context.DeadlineExceeded)
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
