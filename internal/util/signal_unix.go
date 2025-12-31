//go:build !windows

package util

import (
	"io"
	"os"
	"syscall"
)

// ShutdownSignals returns the signals to listen for graceful shutdown.
func ShutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// GracefulSignal attempts graceful process termination.
func GracefulSignal(p *os.Process) error {
	return p.Signal(syscall.SIGINT)
}

// StopFFmpegViaStdin is a no-op on Unix since we use SIGINT.
// Provided for API compatibility with Windows.
func StopFFmpegViaStdin(stdin io.WriteCloser) error {
	// On Unix, FFmpeg is stopped via SIGINT, not stdin.
	// Just close stdin if provided.
	if stdin != nil {
		return stdin.Close()
	}
	return nil
}
