//go:build windows

package util

import (
	"io"
	"log/slog"
	"os"
	"time"
)

// ShutdownSignals returns the signals to listen for graceful shutdown.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// GracefulSignal attempts graceful process termination.
// On Windows, this is a no-op since we use stdin-based shutdown for FFmpeg.
func GracefulSignal(p *os.Process) error {
	// On Windows, we rely on StopFFmpegViaStdin instead.
	// Return nil to avoid adding errors to the shutdown sequence.
	return nil
}

// StopFFmpegViaStdin sends 'q' command to FFmpeg's stdin for graceful shutdown.
// This is the preferred method on Windows where SIGINT is not supported.
// FFmpeg reads keyboard input and 'q' triggers its internal quit handler.
func StopFFmpegViaStdin(stdin io.WriteCloser) error {
	if stdin == nil {
		return nil
	}

	// Send 'q' + newline to trigger FFmpeg's quit command.
	// The newline ensures the command is processed immediately.
	if _, err := stdin.Write([]byte("q\n")); err != nil {
		slog.Debug("failed to send quit command to FFmpeg stdin", "error", err)
	}

	// Brief delay to allow FFmpeg to process the quit command
	// before we close stdin. FFmpeg needs a moment to read and
	// act on the 'q' before the pipe closes.
	time.Sleep(50 * time.Millisecond)

	return stdin.Close()
}
