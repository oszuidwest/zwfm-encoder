// Package types provides shared type definitions used across the encoder.
package types

// FieldError represents a validation error for a specific field.
type FieldError struct {
	Field   string `json:"field"`   // JSON path to the field (e.g., "silence_detection.threshold_db")
	Message string `json:"message"` // Human-readable error message
	Value   any    `json:"value"`   // The invalid value that was provided
}

// ValidationError collects multiple field validation errors.
type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

// NewValidationError creates a new empty ValidationError.
func NewValidationError() *ValidationError {
	return &ValidationError{
		Errors: make([]FieldError, 0),
	}
}

// Add adds a field error to the collection.
func (v *ValidationError) Add(field, message string, value any) {
	v.Errors = append(v.Errors, FieldError{
		Field:   field,
		Message: message,
		Value:   value,
	})
}
