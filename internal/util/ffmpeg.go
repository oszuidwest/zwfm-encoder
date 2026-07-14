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

type ffmpegProtocolProbe func(ffmpegPath string) ([]byte, error)

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
	return probeFFmpegProtocol(
		ffmpegPath,
		protocol,
		runFFmpegProtocolProbe,
	)
}

func probeFFmpegProtocol(
	ffmpegPath string,
	protocol string,
	probe ffmpegProtocolProbe,
) (bool, error) {
	for attempt := 1; attempt <= ffmpegProtocolProbeAttempts; attempt++ {
		out, err := probe(ffmpegPath)
		if err == nil {
			return FFmpegProtocolListContains(string(out), protocol), nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("probe ffmpeg protocols: %w", err)
		}
	}
	return false, fmt.Errorf(
		"probe ffmpeg protocols timed out after %d attempts of %s: %w",
		ffmpegProtocolProbeAttempts,
		ffmpegProtocolProbeTimeout,
		context.DeadlineExceeded,
	)
}

func runFFmpegProtocolProbe(ffmpegPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegProtocolProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output() //nolint:gosec // ffmpegPath is resolved config/PATH.
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, ctx.Err()
	}
	return out, err
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
