package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
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

// EncoderController provides control operations for the audio encoder.
// This interface allows the command handler to control encoder state
// without depending on the concrete Encoder implementation.
type EncoderController interface {
	State() types.EncoderState
	Start() error
	StartOutput(outputID string) error
	StopOutput(outputID string) error
	Restart() error
	TriggerTestWebhook() error
	TriggerTestLog() error
	TriggerTestEmail() error
}

// CommandHandler processes WebSocket commands.
type CommandHandler struct {
	cfg     *config.Config
	encoder EncoderController
}

// NewCommandHandler creates a new command handler.
func NewCommandHandler(cfg *config.Config, encoder EncoderController) *CommandHandler {
	return &CommandHandler{
		cfg:     cfg,
		encoder: encoder,
	}
}

// Handle processes a WebSocket command and performs the requested action.
func (h *CommandHandler) Handle(cmd WSCommand, conn *websocket.Conn, triggerStatusUpdate func()) {
	switch cmd.Type {
	case "add_output":
		h.handleAddOutput(cmd)
	case "delete_output":
		h.handleDeleteOutput(cmd)
	case "update_settings":
		h.handleUpdateSettings(cmd)
	case "test_webhook", "test_log", "test_email":
		h.handleTest(conn, cmd.Type)
	case "view_silence_log":
		h.handleViewSilenceLog(conn)
	default:
		slog.Warn("unknown WebSocket command type", "type", cmd.Type)
	}

	triggerStatusUpdate()
}

func (h *CommandHandler) handleAddOutput(cmd WSCommand) {
	var output types.Output
	if err := json.Unmarshal(cmd.Data, &output); err != nil {
		slog.Warn("add_output: invalid JSON data", "error", err)
		return
	}
	if err := util.ValidateRequired("host", output.Host); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	if err := util.ValidatePort("port", output.Port); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	if err := util.ValidateMaxLength("host", output.Host, MaxHostLength); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	if err := util.ValidateMaxLength("streamid", output.StreamID, MaxStreamIDLength); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	// Limit number of outputs to prevent resource exhaustion
	if len(h.cfg.ConfiguredOutputs()) >= MaxOutputs {
		slog.Warn("add_output: maximum outputs reached", "max", MaxOutputs)
		return
	}
	if err := util.ValidateRange("max_retries", output.MaxRetries, 0, MaxRetriesLimit); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	if output.StreamID == "" {
		output.StreamID = "studio"
	}
	if output.Codec == "" {
		output.Codec = "mp3"
	}
	if err := h.cfg.AddOutput(&output); err != nil {
		slog.Error("add_output: failed to add", "error", err)
		return
	}
	slog.Info("add_output: added output", "host", output.Host, "port", output.Port)
	if h.encoder.State() == types.StateRunning {
		outputs := h.cfg.ConfiguredOutputs()
		if len(outputs) > 0 {
			if err := h.encoder.StartOutput(outputs[len(outputs)-1].ID); err != nil {
				slog.Error("add_output: failed to start output", "error", err)
			}
		}
	}
}

func (h *CommandHandler) handleDeleteOutput(cmd WSCommand) {
	if cmd.ID == "" {
		slog.Warn("delete_output: no ID provided")
		return
	}
	slog.Info("delete_output: deleting", "output_id", cmd.ID)
	if err := h.encoder.StopOutput(cmd.ID); err != nil {
		slog.Error("delete_output: failed to stop", "error", err)
	}
	if err := h.cfg.RemoveOutput(cmd.ID); err != nil {
		slog.Error("delete_output: failed to remove from config", "error", err)
	} else {
		slog.Info("delete_output: removed from config", "output_id", cmd.ID)
	}
}

// updateFloatSetting validates and updates a float64 setting.
func updateFloatSetting(value *float64, minVal, maxVal float64, name string, setter func(float64) error) {
	if value == nil {
		return
	}
	v := *value
	if err := util.ValidateRangeFloat(name, v, minVal, maxVal); err != nil {
		slog.Warn("update_settings: validation failed", "setting", name, "error", err)
		return
	}
	slog.Info("update_settings: changing setting", "setting", name, "value", v)
	if err := setter(v); err != nil {
		slog.Error("update_settings: failed to save", "error", err)
	}
}

// updateStringSetting updates a string setting.
func updateStringSetting(value *string, name string, setter func(string) error) {
	if value == nil {
		return
	}
	slog.Info("update_settings: changing setting", "setting", name)
	if err := setter(*value); err != nil {
		slog.Error("update_settings: failed to save", "error", err)
	}
}

// handleAudioInputChange saves the new audio input and starts/restarts the encoder.
func (h *CommandHandler) handleAudioInputChange(input string) {
	slog.Info("update_settings: changing audio input", "input", input)
	if err := h.cfg.SetAudioInput(input); err != nil {
		slog.Error("update_settings: failed to save audio input", "error", err)
		return
	}
	go func() {
		var err error
		switch h.encoder.State() {
		case types.StateRunning:
			err = h.encoder.Restart()
		case types.StateStopped:
			err = h.encoder.Start()
		}
		if err != nil {
			slog.Error("update_settings: encoder state change failed", "error", err)
		}
	}()
}

func (h *CommandHandler) handleUpdateSettings(cmd WSCommand) {
	var settings struct {
		AudioInput       string   `json:"audio_input"`
		SilenceThreshold *float64 `json:"silence_threshold"`
		SilenceDuration  *float64 `json:"silence_duration"`
		SilenceRecovery  *float64 `json:"silence_recovery"`
		SilenceWebhook   *string  `json:"silence_webhook"`
		SilenceLogPath   *string  `json:"silence_log_path"`
		EmailSMTPHost    *string  `json:"email_smtp_host"`
		EmailSMTPPort    *int     `json:"email_smtp_port"`
		EmailFromName    *string  `json:"email_from_name"`
		EmailUsername    *string  `json:"email_username"`
		EmailPassword    *string  `json:"email_password"`
		EmailRecipients  *string  `json:"email_recipients"`
	}
	if err := json.Unmarshal(cmd.Data, &settings); err != nil {
		slog.Warn("update_settings: invalid JSON data", "error", err)
		return
	}
	if settings.AudioInput != "" {
		h.handleAudioInputChange(settings.AudioInput)
	}
	updateFloatSetting(settings.SilenceThreshold, -60, 0, "silence threshold", h.cfg.SetSilenceThreshold)
	updateFloatSetting(settings.SilenceDuration, 1, 300, "silence duration", h.cfg.SetSilenceDuration)
	updateFloatSetting(settings.SilenceRecovery, 1, 60, "silence recovery", h.cfg.SetSilenceRecovery)
	updateStringSetting(settings.SilenceWebhook, "webhook URL", h.cfg.SetWebhookURL)
	updateStringSetting(settings.SilenceLogPath, "log path", h.cfg.SetLogPath)
	if settings.EmailSMTPHost != nil || settings.EmailSMTPPort != nil ||
		settings.EmailFromName != nil || settings.EmailUsername != nil ||
		settings.EmailPassword != nil || settings.EmailRecipients != nil {
		// Get current values via snapshot (single mutex acquisition)
		snap := h.cfg.Snapshot()
		host := snap.EmailSMTPHost
		port := snap.EmailSMTPPort
		fromName := snap.EmailFromName
		username := snap.EmailUsername
		password := snap.EmailPassword
		recipients := snap.EmailRecipients
		if settings.EmailSMTPHost != nil {
			host = *settings.EmailSMTPHost
		}
		if settings.EmailSMTPPort != nil {
			port = max(1, min(*settings.EmailSMTPPort, 65535))
		}
		if settings.EmailFromName != nil {
			fromName = *settings.EmailFromName
		}
		if settings.EmailUsername != nil {
			username = *settings.EmailUsername
		}
		if settings.EmailPassword != nil {
			password = *settings.EmailPassword
		}
		if settings.EmailRecipients != nil {
			recipients = *settings.EmailRecipients
		}

		slog.Info("update_settings: updating email configuration")
		if err := h.cfg.SetEmailConfig(host, port, fromName, username, password, recipients); err != nil {
			slog.Error("update_settings: failed to save email config", "error", err)
		}
	}
}

// runTest dispatches to the appropriate test method on the encoder.
func (h *CommandHandler) runTest(testType string) error {
	switch testType {
	case "webhook":
		return h.encoder.TriggerTestWebhook()
	case "log":
		return h.encoder.TriggerTestLog()
	case "email":
		return h.encoder.TriggerTestEmail()
	default:
		return fmt.Errorf("unknown test type: %s", testType)
	}
}

// handleTest executes a notification test and sends the result to the client.
// testCmd should be in format "test_<type>" (e.g., "test_email", "test_webhook").
func (h *CommandHandler) handleTest(conn *websocket.Conn, testCmd string) {
	testType := strings.TrimPrefix(testCmd, "test_")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in test handler", "command", testCmd, "panic", r)
			}
		}()

		result := types.WSTestResult{
			Type:     "test_result",
			TestType: testType,
			Success:  true,
		}

		if err := h.runTest(testType); err != nil {
			slog.Error("test failed", "command", testCmd, "error", err)
			result.Success = false
			result.Error = err.Error()
		} else {
			slog.Info("test succeeded", "command", testCmd)
		}

		if wsErr := conn.WriteJSON(result); wsErr != nil {
			slog.Error("failed to send test response", "command", testCmd, "error", wsErr)
		}
	}()
}

// handleViewSilenceLog reads and returns the silence log file contents.
func (h *CommandHandler) handleViewSilenceLog(conn *websocket.Conn) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in silence log handler", "panic", r)
			}
		}()

		result := types.WSSilenceLogResult{
			Type:    "silence_log_result",
			Success: true,
		}

		logPath := h.cfg.LogPath()
		if logPath == "" {
			result.Success = false
			result.Error = "Log file path not configured"
			if wsErr := conn.WriteJSON(result); wsErr != nil {
				slog.Error("failed to send silence log response", "error", wsErr)
			}
			return
		}

		entries, err := readSilenceLog(logPath, MaxLogEntries)
		if err != nil {
			result.Success = false
			result.Error = err.Error()
		} else {
			result.Entries = entries
			result.Path = logPath
		}

		if wsErr := conn.WriteJSON(result); wsErr != nil {
			slog.Error("failed to send silence log response", "error", wsErr)
		}
	}()
}

// readSilenceLog reads the last N entries from the silence log file.
func readSilenceLog(logPath string, maxEntries int) ([]types.SilenceLogEntry, error) {
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return []types.SilenceLogEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read log file: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return []types.SilenceLogEntry{}, nil
	}

	start := max(0, len(lines)-maxEntries)
	lines = lines[start:]

	entries := make([]types.SilenceLogEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry types.SilenceLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // Skip malformed entries
		}
		entries = append(entries, entry)
	}

	// Reverse to show newest first
	slices.Reverse(entries)

	return entries, nil
}
