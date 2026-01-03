package server

import (
	"encoding/json"
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
)

// WSCommand is a command received from a WebSocket client.
// Note: Commands are deprecated. All config operations should use REST API.
type WSCommand struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// CommandHandler is the WebSocket command processor.
// Note: Commands are deprecated. Use REST API instead.
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

// Handle processes a WebSocket command.
// Note: Commands are deprecated. This logs a warning for any received command.
// All config operations should now use the REST API endpoints.
func (h *CommandHandler) Handle(cmd WSCommand, send chan<- any, triggerStatusUpdate func()) {
	slog.Warn("deprecated WebSocket command received, use REST API instead", "type", cmd.Type)
}
