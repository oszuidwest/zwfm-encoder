// Package util provides shared utility functions used across the encoder.
package util

import (
	"io"
	"log/slog"
)

// SafeClose closes an io.Closer and logs any error that occurs.
func SafeClose(closer io.Closer, name string) {
	if closer == nil {
		return
	}
	if err := closer.Close(); err != nil {
		slog.Warn("failed to close resource", "resource", name, "error", err)
	}
}

// SafeCloseFunc creates a defer-friendly closure for resource cleanup.
func SafeCloseFunc(closer io.Closer, name string) func() {
	return func() {
		SafeClose(closer, name)
	}
}
