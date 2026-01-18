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

// StopFFmpegViaStdin closes stdin on Unix systems.
// On Unix, FFmpeg is stopped via SIGINT (see GracefulSignal), so this
// function only closes the stdin pipe to clean up resources.
// Provided for API compatibility with Windows where 'q' stdin is required.
func StopFFmpegViaStdin(stdin io.WriteCloser) error {
	if stdin == nil {
		return nil
	}
	return stdin.Close()
}
