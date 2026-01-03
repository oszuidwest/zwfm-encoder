// Package ffmpeg provides shared FFmpeg process management utilities.
package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// Process represents a running FFmpeg subprocess.
type Process struct {
	Cmd    *exec.Cmd
	Cancel context.CancelFunc
	Stdin  io.WriteCloser
	Stderr *bytes.Buffer
}

// BaseInputArgs returns FFmpeg arguments for PCM audio input.
func BaseInputArgs() []string {
	return []string{
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", types.SampleRate),
		"-ac", fmt.Sprintf("%d", types.Channels),
		"-i", "pipe:0",
	}
}

// StartProcess launches an FFmpeg subprocess.
func StartProcess(ffmpegPath string, args []string) (*Process, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		if closeErr := stdinPipe.Close(); closeErr != nil {
			slog.Warn("failed to close stdin pipe", "error", closeErr)
		}
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	return &Process{
		Cmd:    cmd,
		Cancel: cancel,
		Stdin:  stdinPipe,
		Stderr: &stderr,
	}, nil
}
