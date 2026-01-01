package util

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidationError represents a field validation failure.
type ValidationError struct {
	Field   string
	Message string
}

// Error returns a formatted validation error message.
func (v *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", v.Field, v.Message)
}

// ValidateRequired returns an error if the string field is empty.
func ValidateRequired(field, value string) *ValidationError {
	if value == "" {
		return &ValidationError{Field: field, Message: fmt.Sprintf("%s is required", field)}
	}
	return nil
}

// ValidateRange returns an error if value is outside the range [minVal, maxVal].
func ValidateRange(field string, value, minVal, maxVal int) *ValidationError {
	if value < minVal || value > maxVal {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s must be between %d and %d, got %d", field, minVal, maxVal, value),
		}
	}
	return nil
}

// ValidateRangeFloat returns an error if value is outside the range [minVal, maxVal].
func ValidateRangeFloat(field string, value, minVal, maxVal float64) *ValidationError {
	if value < minVal || value > maxVal {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s must be between %.1f and %.1f, got %.1f", field, minVal, maxVal, value),
		}
	}
	return nil
}

// ValidateMaxLength returns an error if value exceeds maxLen characters.
func ValidateMaxLength(field, value string, maxLen int) *ValidationError {
	if len(value) > maxLen {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s too long (max %d chars)", field, maxLen),
		}
	}
	return nil
}

// ValidatePort returns an error if port is not in the valid range 1-65535.
func ValidatePort(field string, port int) *ValidationError {
	return ValidateRange(field, port, 1, 65535)
}

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
// It cleans the path and rejects path traversal attempts (.. components).
// Returns nil if the path is valid.
func ValidatePath(field, path string) *ValidationError {
	if path == "" {
		return &ValidationError{Field: field, Message: fmt.Sprintf("%s is required", field)}
	}

	// Reject path traversal attempts before cleaning
	// This catches both explicit "../" and encoded variants
	if strings.Contains(path, "..") {
		return &ValidationError{Field: field, Message: "path cannot contain '..'"}
	}

	// Clean the path to normalize it
	cleaned := filepath.Clean(path)

	// After cleaning, verify no traversal components remain
	// (filepath.Clean converts "a/../b" to "b", but we already rejected "..")
	if strings.Contains(cleaned, "..") {
		return &ValidationError{Field: field, Message: "invalid path"}
	}

	return nil
}
