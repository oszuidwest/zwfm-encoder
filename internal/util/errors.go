package util

import (
	"fmt"
	"strings"
)

// maxErrorLineLength is the maximum length for extracted error messages.
const maxErrorLineLength = 200

// WrapError wraps an error with a descriptive operation context.
func WrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("failed to %s: %w", operation, err)
}

// ExtractLastError extracts the last meaningful line from stderr output.
func ExtractLastError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > maxErrorLineLength {
				return line[:maxErrorLineLength] + "..."
			}
			return line
		}
	}
	return ""
}
