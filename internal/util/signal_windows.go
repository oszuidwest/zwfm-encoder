//go:build windows

package util

import (
	"errors"
	"os"
)

// ErrGracefulNotSupported indicates graceful shutdown is not supported.
var ErrGracefulNotSupported = errors.New("graceful signal not supported on Windows")

// ShutdownSignals returns the signals to listen for graceful shutdown.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// GracefulSignal attempts graceful process termination.
// On Windows, returns an error to trigger the WaitDelay then kill fallback.
func GracefulSignal(p *os.Process) error {
	// Return error so exec.Cmd will wait WaitDelay, then kill.
	// This is safer than immediate kill - gives stdin EOF time to work.
	return ErrGracefulNotSupported
}

// ForceKill forcefully terminates a process.
func ForceKill(p *os.Process) error {
	return p.Signal(os.Kill)
}
