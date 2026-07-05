//go:build !windows

package util

// SetupLogging is a no-op on Unix - the default slog handler writes to stderr,
// which is always attached when the process is launched by systemd or a shell.
func SetupLogging() {}
