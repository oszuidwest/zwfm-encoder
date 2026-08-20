package util

import (
	"fmt"
	"slices"
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
	for _, line := range slices.Backward(lines) {
		line := strings.TrimSpace(line)
		if line != "" {
			if len(line) > maxErrorLineLength {
				return line[:maxErrorLineLength] + "..."
			}
			return line
		}
	}
	return ""
}
