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
// Commands use slash-style format: namespace/action (e.g., "outputs/add", "audio/update")
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
	case "notifications":
		h.handleNotifications(action, subaction, cmd, send)
	case "recording":
		h.handleRecording(action, cmd, send)
	case "config":
		h.handleConfig(action, send)
	case "status":
		h.handleStatus(action, send)
	default:
		slog.Warn("unknown WebSocket command", "type", cmd.Type)
	}

	triggerStatusUpdate()
}

// --- Namespace handlers ---

// handleOutputs routes outputs/* commands
func (h *CommandHandler) handleOutputs(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "add":
		h.handleAddOutput(cmd, send)
	case "delete":
		h.handleDeleteOutput(cmd, send)
	case "update":
		h.handleUpdateOutput(cmd, send)
	case "clear-error":
		h.handleClearOutputError(cmd, send)
	default:
		slog.Warn("unknown outputs action", "action", action)
	}
}

// handleRecorders routes recorders/* commands
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
	case "clear-error":
		h.handleClearRecorderError(cmd, send)
	case "test-s3":
		h.handleTestRecorderS3(cmd, send)
	default:
		slog.Warn("unknown recorders action", "action", action)
	}
}

// handleAudio routes audio/* commands
func (h *CommandHandler) handleAudio(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "update":
		h.handleAudioUpdate(cmd, send)
	case "get":
		h.handleAudioGet(send)
	default:
		slog.Warn("unknown audio action", "action", action)
	}
}

// handleSilence routes silence/* commands
func (h *CommandHandler) handleSilence(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "update":
		h.handleSilenceUpdate(cmd, send)
	case "get":
		h.handleSilenceGet(send)
	default:
		slog.Warn("unknown silence action", "action", action)
	}
}

// handleNotifications routes notifications/*/* commands
func (h *CommandHandler) handleNotifications(action, subaction string, cmd WSCommand, send chan<- any) {
	switch action {
	case "webhook":
		switch subaction {
		case "update":
			h.handleWebhookUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_webhook")
		case "get":
			h.handleWebhookGet(send)
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
		case "get":
			h.handleLogGet(send)
		default:
			slog.Warn("unknown log action", "subaction", subaction)
		}
	case "email":
		switch subaction {
		case "update":
			h.handleEmailUpdate(cmd, send)
		case "test":
			h.handleTest(send, "test_email")
		case "get":
			h.handleEmailGet(send)
		default:
			slog.Warn("unknown email action", "subaction", subaction)
		}
	default:
		slog.Warn("unknown notifications action", "action", action)
	}
}

// handleRecording routes recording/* commands
func (h *CommandHandler) handleRecording(action string, cmd WSCommand, send chan<- any) {
	switch action {
	case "regenerate-key":
		h.handleRegenerateAPIKey(send)
	case "get":
		h.handleRecordingGet(send)
	default:
		slog.Warn("unknown recording action", "action", action)
	}
}

// handleConfig routes config/* commands
func (h *CommandHandler) handleConfig(action string, send chan<- any) {
	switch action {
	case "get":
		h.handleConfigGet(send)
	default:
		slog.Warn("unknown config action", "action", action)
	}
}

// handleStatus routes status/* commands
func (h *CommandHandler) handleStatus(action string, send chan<- any) {
	switch action {
	case "get":
		// Status is sent automatically, but explicit get triggers immediate update
		slog.Debug("status/get received, status update will be triggered")
	default:
		slog.Warn("unknown status action", "action", action)
	}
}
