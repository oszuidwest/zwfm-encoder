// Package types provides shared type definitions used across the encoder.
package types

import (
	"fmt"
	"strings"
)

// FieldError represents a validation error for a specific field.
type FieldError struct {
	Field   string `json:"field"`   // JSON path to the field (e.g., "silence_detection.threshold_db")
	Message string `json:"message"` // Human-readable error message
	Value   any    `json:"value"`   // The invalid value that was provided
}

// Error implements the error interface.
func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
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

// AddError adds a FieldError to the collection.
func (v *ValidationError) AddError(err FieldError) {
	v.Errors = append(v.Errors, err)
}

// Merge combines errors from another ValidationError.
func (v *ValidationError) Merge(other *ValidationError) {
	if other != nil {
		v.Errors = append(v.Errors, other.Errors...)
	}
}

// HasErrors returns true if there are any validation errors.
func (v *ValidationError) HasErrors() bool {
	return len(v.Errors) > 0
}

// Error implements the error interface.
func (v *ValidationError) Error() string {
	if !v.HasErrors() {
		return ""
	}
	var msgs []string
	for _, e := range v.Errors {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "; ")
}

// FirstError returns the first error message, or empty string if no errors.
func (v *ValidationError) FirstError() string {
	if !v.HasErrors() {
		return ""
	}
	return v.Errors[0].Message
}

// AsError returns the ValidationError as an error interface, or nil if no errors.
func (v *ValidationError) AsError() error {
	if !v.HasErrors() {
		return nil
	}
	return v
}
