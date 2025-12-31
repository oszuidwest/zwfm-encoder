//go:build windows

package util

import (
	"io"
	"os"
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
func StopFFmpegViaStdin(stdin io.WriteCloser) error {
	if stdin == nil {
		return nil
	}
	// Send 'q' to trigger FFmpeg's quit command
	_, _ = stdin.Write([]byte("q"))
	return stdin.Close()
}
