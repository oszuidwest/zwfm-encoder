package server

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
)

// Validation limits for output configuration.
const (
	MaxOutputs    = 10  // Maximum concurrent outputs
	MaxLogEntries = 100 // Maximum silence log entries to return
)

// WSCommand is a command received from a WebSocket client.
type WSCommand struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// CommandHandler is the WebSocket command processor for the encoder.
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
func (h *CommandHandler) Handle(cmd WSCommand, send chan<- any, triggerStatusUpdate func()) {
	// Parse command into namespace and action
	parts := strings.SplitN(cmd.Type, "/", 3)
	namespace := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	subaction := ""
	if len(parts) > 2 {
		subaction = parts[2]
	}

	switch namespace {
	case "outputs":
		h.handleOutputs(action, cmd, send)
	case "recorders":
		h.handleRecorders(action, cmd, send)
	case "audio":
		h.handleAudio(action, cmd, send)
	case "silence":
		h.handleSilence(action, cmd, send)
	case "silence_dump":
		h.handleSilenceDump(action, cmd, send)
	case "notifications":
		h.handleNotifications(action, subaction, cmd, send)
	case "recording":
		h.handleRecording(action, cmd, send)
	default:
		slog.Warn("unknown WebSocket command", "type", cmd.Type)
	}

	triggerStatusUpdate()
}

// --- Namespace handlers ---

// handleOutputs routes outputs/* commands.
func (h *CommandHandler) handleOutputs(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "add":
		h.handleAddOutput(cmd, send)
	case "delete":
		h.handleDeleteOutput(cmd, send)
	case "update":
		h.handleUpdateOutput(cmd, send)
	default:
		slog.Warn("unknown outputs action", "action", action)
	}
}

// handleRecorders routes recorders/* commands.
func (h *CommandHandler) handleRecorders(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "add":
		h.handleAddRecorder(cmd, send)
	case "delete":
		h.handleDeleteRecorder(cmd, send)
	case "update":
		h.handleUpdateRecorder(cmd, send)
	case "start":
		h.handleStartRecorder(cmd, send)
	case "stop":
		h.handleStopRecorder(cmd, send)
	case "test-s3":
		h.handleTestRecorderS3(cmd, send)
	default:
		slog.Warn("unknown recorders action", "action", action)
	}
}

// handleAudio routes audio/* commands.
func (h *CommandHandler) handleAudio(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "update":
		h.handleAudioUpdate(cmd, send)
	default:
		slog.Warn("unknown audio action", "action", action)
	}
}

// handleSilence routes silence/* commands.
func (h *CommandHandler) handleSilence(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "update":
		h.handleSilenceUpdate(cmd, send)
	default:
		slog.Warn("unknown silence action", "action", action)
	}
}

// handleSilenceDump routes silence_dump/* commands.
func (h *CommandHandler) handleSilenceDump(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "update":
		h.handleSilenceDumpUpdate(cmd, send)
	default:
		slog.Warn("unknown silence_dump action", "action", action)
	}
}

// handleNotifications routes notifications/*/* commands.
func (h *CommandHandler) handleNotifications(action, subaction string, cmd WSCommand, send chan<- any) {
	switch action {
	case "webhook":
		switch subaction {
		case "update":
			h.handleWebhookUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_webhook")
		default:
			slog.Warn("unknown webhook action", "subaction", subaction)
		}
	case "log":
		switch subaction {
		case "update":
			h.handleLogUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_log")
		case "view":
			h.handleViewSilenceLog(send)
		default:
			slog.Warn("unknown log action", "subaction", subaction)
		}
	case "email":
		switch subaction {
		case "update":
			h.handleEmailUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_email")
		default:
			slog.Warn("unknown email action", "subaction", subaction)
		}
	case "zabbix":
		switch subaction {
		case "update":
			h.handleZabbixUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_zabbix")
		default:
			slog.Warn("unknown zabbix action", "subaction", subaction)
		}
	default:
		slog.Warn("unknown notifications action", "action", action)
	}
}

// handleRecording routes recording/* commands.
func (h *CommandHandler) handleRecording(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "regenerate-key":
		h.handleRegenerateAPIKey(send)
	default:
		slog.Warn("unknown recording action", "action", action)
	}
}
