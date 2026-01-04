package util

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IsConfigured reports whether all provided values are non-empty.
func IsConfigured(values ...string) bool {
	for _, v := range values {
		if v == "" {
			return false
		}
	}
	return true
}

// ValidatePath validates a file path for security.
func ValidatePath(field, path string) error {
	if path == "" {
		return fmt.Errorf("%s: is required", field)
	}

	// Reject path traversal attempts before cleaning
	// This catches both explicit "../" and encoded variants
	if strings.Contains(path, "..") {
		return fmt.Errorf("%s: path cannot contain '..'", field)
	}

	// Clean the path to normalize it
	cleaned := filepath.Clean(path)

	// After cleaning, verify no traversal components remain
	// (filepath.Clean converts "a/../b" to "b", but we already rejected "..")
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("%s: invalid path", field)
	}

	return nil
}

// CheckPathWritable verifies that a directory path exists and is writable.
func CheckPathWritable(path string) error {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(path, 0o755); err != nil {
		slog.Error("path writability check failed", "path", path, "error", err, "step", "mkdir")
		return fmt.Errorf("path is not writable")
	}

	// Create test file with unique name using nanosecond timestamp
	testFile := filepath.Join(path, fmt.Sprintf(".encoder-write-test-%d", time.Now().UnixNano()))

	f, err := os.Create(testFile)
	if err != nil {
		slog.Error("path writability check failed", "path", path, "error", err, "step", "create")
		return fmt.Errorf("path is not writable")
	}

	// Write 1KB of data to verify actual write capability
	data := make([]byte, 1024)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(testFile) // Best effort cleanup
		slog.Error("path writability check failed", "path", path, "error", err, "step", "write")
		return fmt.Errorf("path is not writable")
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(testFile) // Best effort cleanup
		slog.Error("path writability check failed", "path", path, "error", err, "step", "close")
		return fmt.Errorf("path is not writable")
	}

	// Cleanup must succeed for the check to pass
	if err := os.Remove(testFile); err != nil {
		slog.Error("path writability check failed", "path", path, "error", err, "step", "remove")
		return fmt.Errorf("path is not writable")
	}

	return nil
}
