//go:build !windows

package util

import (
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
