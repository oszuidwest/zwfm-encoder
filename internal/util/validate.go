package util

import "fmt"

// ValidationError represents a field validation failure.
// It implements the error interface for idiomatic error handling.
type ValidationError struct {
	Field   string
	Message string
}

// Error implements the error interface.
func (v *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", v.Field, v.Message)
}

// ValidateRequired checks that a string field is not empty.
func ValidateRequired(field, value string) *ValidationError {
	if value == "" {
		return &ValidationError{Field: field, Message: fmt.Sprintf("%s is required", field)}
	}
	return nil
}

// ValidateRange checks that an integer is within bounds (inclusive).
func ValidateRange(field string, value, minVal, maxVal int) *ValidationError {
	if value < minVal || value > maxVal {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s must be between %d and %d, got %d", field, minVal, maxVal, value),
		}
	}
	return nil
}

// ValidateRangeFloat checks that a float64 is within bounds (inclusive).
func ValidateRangeFloat(field string, value, minVal, maxVal float64) *ValidationError {
	if value < minVal || value > maxVal {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s must be between %.1f and %.1f, got %.1f", field, minVal, maxVal, value),
		}
	}
	return nil
}

// ValidateMaxLength checks that a string doesn't exceed max length.
func ValidateMaxLength(field, value string, maxLen int) *ValidationError {
	if len(value) > maxLen {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%s too long (max %d chars)", field, maxLen),
		}
	}
	return nil
}

// ValidatePort checks that a port number is valid (1-65535).
func ValidatePort(field string, port int) *ValidationError {
	return ValidateRange(field, port, 1, 65535)
}

// IsConfigured returns true if all provided values are non-empty.
func IsConfigured(values ...string) bool {
	for _, v := range values {
		if v == "" {
			return false
		}
	}
	return true
}
