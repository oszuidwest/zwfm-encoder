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
	"strconv"
	"sync"
	"time"

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
	stdout io.ReadCloser

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
		"-ar", strconv.Itoa(audio.SampleRate),
		"-ac", strconv.Itoa(audio.Channels),
		"-i", "pipe:0",
	}
}

// StartProcess launches an FFmpeg subprocess.
func StartProcess(ffmpegPath string, args []string) (*StartResult, error) {
	return startProcess(ffmpegPath, args, false)
}

// StartProcessWithStdout launches an FFmpeg subprocess with stdout available through [StartResult.Stdout].
func StartProcessWithStdout(ffmpegPath string, args []string) (*StartResult, error) {
	return startProcess(ffmpegPath, args, true)
}

func startProcess(ffmpegPath string, args []string, captureStdout bool) (*StartResult, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	//nolint:gosec // G204: ffmpegPath is from config or PATH lookup, not user HTTP input
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel(fmt.Errorf("create stdin pipe: %w", err))
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	var stdoutPipe io.ReadCloser
	if captureStdout {
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			cancel(fmt.Errorf("create stdout pipe: %w", err))
			if closeErr := stdinPipe.Close(); closeErr != nil {
				slog.Warn("failed to close stdin pipe", "error", closeErr)
			}
			return nil, fmt.Errorf("create stdout pipe: %w", err)
		}
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
		stdout:   stdoutPipe,
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

	// Wait may close stdin first; late CloseStdin calls are harmless.
	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
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

// EscalateUntil waits for done to close, escalating shutdown when it lingers:
// after signalTimeout it sends a graceful signal, after another killTimeout it
// kills the process, then waits for done unconditionally. The onSignal and
// onKill callbacks run in the caller's goroutine just before each escalation
// so call sites can log their own context.
func (r *StartResult) EscalateUntil(done <-chan struct{}, signalTimeout, killTimeout time.Duration, onSignal, onKill func()) {
	select {
	case <-done:
		return
	case <-time.After(signalTimeout):
		onSignal()
		_ = r.Signal()
	}
	select {
	case <-done:
		return
	case <-time.After(killTimeout):
		onKill()
		_ = r.Kill()
	}
	<-done
}

// WaitEscalating waits for the process to exit with Signal/Kill escalation and
// returns the process exit error. See [StartResult.EscalateUntil].
func (r *StartResult) WaitEscalating(signalTimeout, killTimeout time.Duration, onSignal, onKill func()) error {
	done := make(chan struct{})
	go func() {
		_ = r.Wait()
		close(done)
	}()
	r.EscalateUntil(done, signalTimeout, killTimeout, onSignal, onKill)
	return r.Wait() // returns the cached result immediately
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

// Stdout returns the process stdout reader, or nil when stdout was not captured.
func (r *StartResult) Stdout() io.Reader {
	if r == nil {
		return nil
	}
	return r.stdout
}
