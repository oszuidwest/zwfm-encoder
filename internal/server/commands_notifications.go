package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

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
func (h *CommandHandler) handleTest(send chan<- interface{}, testCmd string) {
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

		// Send via channel (non-blocking to prevent goroutine leak if channel is closed)
		select {
		case send <- result:
		default:
			slog.Warn("failed to send test response: channel full or closed", "command", testCmd)
		}
	}()
}

// handleViewSilenceLog reads and returns the silence log file contents.
func (h *CommandHandler) handleViewSilenceLog(send chan<- interface{}) {
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
		} else {
			entries, err := readSilenceLog(logPath, MaxLogEntries)
			if err != nil {
				result.Success = false
				result.Error = err.Error()
			} else {
				result.Entries = entries
				result.Path = logPath
			}
		}

		// Send via channel (non-blocking to prevent goroutine leak if channel is closed)
		select {
		case send <- result:
		default:
			slog.Warn("failed to send silence log response: channel full or closed")
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
