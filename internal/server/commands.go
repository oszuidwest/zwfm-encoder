package server

import (
	"encoding/json"
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
)

// Validation limits for output configuration.
const (
	MaxHostLength     = 253  // RFC 1035 hostname limit
	MaxStreamIDLength = 256  // SRT stream ID limit
	MaxOutputs        = 10   // Maximum concurrent outputs
	MaxRetriesLimit   = 9999 // Upper bound for retry configuration
	MaxLogEntries     = 100  // Maximum silence log entries to return
)

// WSCommand is a command received from a WebSocket client.
type WSCommand struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// CommandHandler processes WebSocket commands.
type CommandHandler struct {
	cfg             *config.Config
	encoder         *encoder.Encoder
	ffmpegAvailable bool
}

// NewCommandHandler creates a new command handler.
func NewCommandHandler(cfg *config.Config, enc *encoder.Encoder, ffmpegAvailable bool) *CommandHandler {
	return &CommandHandler{
		cfg:             cfg,
		encoder:         enc,
		ffmpegAvailable: ffmpegAvailable,
	}
}

// Handle processes a WebSocket command and performs the requested action.
// The send channel is used for thread-safe communication back to the client.
func (h *CommandHandler) Handle(cmd WSCommand, send chan<- interface{}, triggerStatusUpdate func()) {
	switch cmd.Type {
	case "add_output":
		h.handleAddOutput(cmd)
	case "delete_output":
		h.handleDeleteOutput(cmd)
	case "update_output":
		h.handleUpdateOutput(cmd)
	case "update_settings":
		h.handleUpdateSettings(cmd)
	case "test_webhook", "test_log", "test_email":
		h.handleTest(send, cmd.Type)
	case "view_silence_log":
		h.handleViewSilenceLog(send)
	case "add_recorder":
		h.handleAddRecorder(cmd, send)
	case "delete_recorder":
		h.handleDeleteRecorder(cmd, send)
	case "update_recorder":
		h.handleUpdateRecorder(cmd, send)
	case "start_recorder":
		h.handleStartRecorder(cmd, send)
	case "stop_recorder":
		h.handleStopRecorder(cmd, send)
	case "test_recorder_s3":
		h.handleTestRecorderS3(cmd, send)
	case "regenerate_api_key":
		h.handleRegenerateAPIKey(send)
	default:
		slog.Warn("unknown WebSocket command type", "type", cmd.Type)
	}

	triggerStatusUpdate()
}
