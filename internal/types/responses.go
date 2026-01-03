package types

// WSConfigResponse is sent in response to config/get.
// Contains the full configuration without runtime state.
type WSConfigResponse struct {
	Type   string      `json:"type"` // "config"
	Config interface{} `json:"config"`
}

// WSCommandResult is the standard response for command execution.
// Used by new slash-style commands (audio/update, silence/update, etc.)
type WSCommandResult struct {
	Type    string           `json:"type"`            // "<command>_result"
	Success bool             `json:"success"`         // true if command succeeded
	Error   *ValidationError `json:"error,omitempty"` // Validation errors if failed
	Data    interface{}      `json:"data,omitempty"`  // Optional response data
}
