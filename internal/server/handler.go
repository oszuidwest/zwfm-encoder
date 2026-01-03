// Package server provides HTTP and WebSocket handlers for the encoder web interface.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// validate is a singleton validator instance.
// It caches struct information for performance.
var validate *validator.Validate

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())

	// Use JSON tag names in error messages instead of struct field names
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return fld.Name
		}
		return name
	})
}

// HandleCommand is a generic command handler that decodes, validates, and processes a command.
// It handles all the boilerplate: JSON decoding, struct validation, error responses.
//
// Type parameter T is the request data struct (must have validation tags).
// The process function receives the validated data and returns an error if processing fails.
func HandleCommand[T any](h *CommandHandler, cmd WSCommand, send chan<- any, process func(*T) error) {
	var data T
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		SendError(send, cmd.Type, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	if err := validate.Struct(data); err != nil {
		SendValidationErrors(send, cmd.Type, err)
		return
	}

	if err := process(&data); err != nil {
		SendError(send, cmd.Type, err)
		return
	}

	SendSuccess(send, cmd.Type, nil)
}

// HandleActionAsync handles async commands with panic recovery (for I/O operations).
func HandleActionAsync(cmd WSCommand, send chan<- any, action func() (any, error)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in async handler", "command", cmd.Type, "panic", r)
				SendError(send, cmd.Type, fmt.Errorf("internal error"))
			}
		}()

		result, err := action()
		if err != nil {
			SendError(send, cmd.Type, err)
			return
		}
		SendSuccess(send, cmd.Type, result)
	}()
}

// --- Response helpers ---

// SendSuccess sends a success response for a command.
func SendSuccess(send chan<- any, cmdType string, data any) {
	result := map[string]any{
		"type":    cmdType + "_result",
		"success": true,
	}
	if data != nil {
		result["data"] = data
	}
	trySend(send, cmdType, result)
}

// SendError sends an error response for a command.
func SendError(send chan<- any, cmdType string, err error) {
	result := map[string]any{
		"type":    cmdType + "_result",
		"success": false,
		"error":   err.Error(),
	}
	trySend(send, cmdType, result)
}

// SendValidationErrors converts validator errors to our format and sends them.
func SendValidationErrors(send chan<- any, cmdType string, err error) {
	verr := types.NewValidationError()

	if validationErrors, ok := err.(validator.ValidationErrors); ok {
		for _, e := range validationErrors {
			field := e.Field()
			msg := formatValidationMessage(e)
			verr.Add(field, msg, e.Value())
		}
	} else {
		// Fallback for non-validation errors
		verr.Add("", err.Error(), nil)
	}

	result := map[string]any{
		"type":    cmdType + "_result",
		"success": false,
		"error":   verr,
	}
	trySend(send, cmdType, result)
}

// SendData sends arbitrary data (for custom response types like test_result).
func SendData(send chan<- any, data any) {
	trySend(send, "data", data)
}

// trySend attempts to send a message, logging a warning if the channel is full.
func trySend(send chan<- any, cmdType string, msg any) {
	select {
	case send <- msg:
	default:
		slog.Warn("failed to send response: channel full or closed", "type", cmdType)
	}
}

// formatValidationMessage creates a human-readable message from a validator error.
func formatValidationMessage(e validator.FieldError) string {
	switch e.Tag() {
	case "required":
		return "is required"
	case "min":
		return fmt.Sprintf("must be at least %s", e.Param())
	case "max":
		return fmt.Sprintf("must be at most %s", e.Param())
	case "gte":
		return fmt.Sprintf("must be greater than or equal to %s", e.Param())
	case "lte":
		return fmt.Sprintf("must be less than or equal to %s", e.Param())
	case "url":
		return "must be a valid URL"
	case "email":
		return "must be a valid email address"
	case "oneof":
		return fmt.Sprintf("must be one of: %s", e.Param())
	case "hostname":
		return "must be a valid hostname"
	case "hostname_port":
		return "must be a valid host:port"
	case "required_with":
		return fmt.Sprintf("is required when %s is set", e.Param())
	case "required_without":
		return fmt.Sprintf("is required when %s is not set", e.Param())
	default:
		return fmt.Sprintf("failed validation '%s'", e.Tag())
	}
}

// --- Entity result helpers (for outputs/recorders with action field) ---

// SendEntityResult sends an entity operation result (add/update/delete).
func SendEntityResult(send chan<- any, entityType, action, id string, success bool, errMsg string) {
	result := struct {
		Type    string `json:"type"`
		Action  string `json:"action"`
		ID      string `json:"id,omitempty"`
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}{
		Type:    entityType + "_result",
		Action:  action,
		ID:      id,
		Success: success,
		Error:   errMsg,
	}
	trySend(send, entityType, result)
}
