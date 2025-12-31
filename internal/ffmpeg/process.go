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

// Process holds FFmpeg process state.
type Process struct {
	Cmd    *exec.Cmd
	Ctx    context.Context
	Cancel context.CancelFunc
	Stdin  io.WriteCloser
	Stderr *bytes.Buffer
}

// BaseInputArgs returns the common FFmpeg input arguments for PCM audio.
func BaseInputArgs() []string {
	return []string{
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", types.SampleRate),
		"-ac", fmt.Sprintf("%d", types.Channels),
		"-i", "pipe:0",
	}
}

// StartProcess creates and starts an FFmpeg process with the given arguments.
// Returns a Process struct containing the command, context, stdin pipe, and stderr buffer.
// The caller is responsible for calling Cancel() and closing Stdin when done.
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
		Ctx:    ctx,
		Cancel: cancel,
		Stdin:  stdinPipe,
		Stderr: &stderr,
	}, nil
}

// Close gracefully closes the process by closing stdin and canceling the context.
func (p *Process) Close() {
	if p.Stdin != nil {
		if err := p.Stdin.Close(); err != nil {
			slog.Warn("failed to close stdin", "error", err)
		}
	}
	if p.Cancel != nil {
		p.Cancel()
	}
}
