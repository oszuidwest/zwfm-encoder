// Package ffmpeg provides shared FFmpeg process management utilities.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// ErrStdinClosed is returned when writing to a closed stdin pipe.
var ErrStdinClosed = errors.New("stdin closed")

// StartResult contains a started FFmpeg subprocess with thread-safe access.
// All fields are private - use methods for controlled access.
type StartResult struct {
	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelCauseFunc
	stderr *bytes.Buffer

	stdin   io.WriteCloser
	stdinMu sync.Mutex // protects stdin field

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}
}

// BaseInputArgs returns FFmpeg arguments for PCM audio input.
func BaseInputArgs() []string {
	return []string{
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", audio.SampleRate),
		"-ac", fmt.Sprintf("%d", audio.Channels),
		"-i", "pipe:0",
	}
}

// StartProcess launches an FFmpeg subprocess.
func StartProcess(ffmpegPath string, args []string) (*StartResult, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel(fmt.Errorf("create stdin pipe: %w", err))
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel(fmt.Errorf("start ffmpeg: %w", err))
		if closeErr := stdinPipe.Close(); closeErr != nil {
			slog.Warn("failed to close stdin pipe", "error", closeErr)
		}
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	return &StartResult{
		cmd:      cmd,
		ctx:      ctx,
		cancel:   cancel,
		stderr:   &stderr,
		stdin:    stdinPipe,
		waitDone: make(chan struct{}),
	}, nil
}

// WriteStdin writes data to the process stdin in a thread-safe manner.
// Returns ErrStdinClosed if stdin has been closed.
func (r *StartResult) WriteStdin(data []byte) (int, error) {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()

	if r.stdin == nil {
		return 0, ErrStdinClosed
	}

	return r.stdin.Write(data)
}

// CloseStdin closes stdin in a thread-safe manner.
// Safe to call multiple times - subsequent calls are no-ops.
func (r *StartResult) CloseStdin() {
	r.stdinMu.Lock()
	stdin := r.stdin
	r.stdin = nil
	r.stdinMu.Unlock()

	if stdin == nil {
		return
	}

	if err := stdin.Close(); err != nil {
		slog.Warn("failed to close stdin", "error", err)
	}
}

// Wait blocks until the FFmpeg process exits and returns any error.
// Safe for concurrent calls: the first caller runs cmd.Wait(), subsequent
// callers receive the cached result.
func (r *StartResult) Wait() error {
	r.waitOnce.Do(func() {
		r.waitErr = r.cmd.Wait()
		close(r.waitDone)
	})
	<-r.waitDone
	return r.waitErr
}

// Signal sends SIGTERM (Unix) or soft termination (Windows) for graceful shutdown.
func (r *StartResult) Signal() error {
	if r.cmd.Process == nil {
		return nil
	}
	return util.GracefulSignal(r.cmd.Process)
}

// Kill forcefully terminates the process with SIGKILL.
func (r *StartResult) Kill() error {
	if r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Kill()
}

// Cancel cancels the process context with the given cause.
// This triggers exec.CommandContext to terminate the process.
func (r *StartResult) Cancel(cause error) {
	if r.cancel != nil {
		r.cancel(cause)
	}
}

// Context returns the process context for cause inspection.
func (r *StartResult) Context() context.Context {
	return r.ctx
}

// Stderr returns the captured stderr output.
// Should only be called after Wait() returns to avoid data races.
func (r *StartResult) Stderr() string {
	if r.stderr == nil {
		return ""
	}
	return r.stderr.String()
}

// Process returns the underlying os.Process for advanced operations.
// Use with caution - prefer the provided methods when possible.
func (r *StartResult) Process() *os.Process {
	return r.cmd.Process
}
