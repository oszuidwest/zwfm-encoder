package server

import (
	"encoding/json"
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

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

// updateSecondsToMsSetting validates a seconds value and stores it as milliseconds.
func updateSecondsToMsSetting(value *float64, minSec, maxSec float64, name string, setter func(int64) error) {
	if value == nil {
		return
	}
	v := *value
	if err := util.ValidateRangeFloat(name, v, minSec, maxSec); err != nil {
		slog.Warn("update_settings: validation failed", "setting", name, "error", err)
		return
	}
	ms := int64(v * 1000)
	slog.Info("update_settings: changing setting", "setting", name, "seconds", v, "ms", ms)
	if err := setter(ms); err != nil {
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
	// Don't start/restart encoder if FFmpeg is not available
	if !h.ffmpegAvailable {
		slog.Warn("update_settings: FFmpeg not available, encoder will not start")
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
		AudioInput        string   `json:"audio_input"`
		SilenceThreshold  *float64 `json:"silence_threshold"`
		SilenceDuration   *float64 `json:"silence_duration"`
		SilenceRecovery   *float64 `json:"silence_recovery"`
		SilenceWebhook    *string  `json:"silence_webhook"`
		SilenceLogPath    *string  `json:"silence_log_path"`
		GraphTenantID     *string  `json:"graph_tenant_id"`
		GraphClientID     *string  `json:"graph_client_id"`
		GraphClientSecret *string  `json:"graph_client_secret"`
		GraphFromAddress  *string  `json:"graph_from_address"`
		GraphRecipients   *string  `json:"graph_recipients"`
		RecordingAPIKey   *string  `json:"recording_api_key"`
	}
	if err := json.Unmarshal(cmd.Data, &settings); err != nil {
		slog.Warn("update_settings: invalid JSON data", "error", err)
		return
	}
	if settings.AudioInput != "" {
		h.handleAudioInputChange(settings.AudioInput)
	}
	updateFloatSetting(settings.SilenceThreshold, -60, 0, "silence threshold", h.cfg.SetSilenceThreshold)
	updateSecondsToMsSetting(settings.SilenceDuration, 0.5, 300, "silence duration", h.cfg.SetSilenceDurationMs)
	updateSecondsToMsSetting(settings.SilenceRecovery, 0.5, 60, "silence recovery", h.cfg.SetSilenceRecoveryMs)
	// Notify encoder to apply new silence detection settings immediately
	if settings.SilenceThreshold != nil || settings.SilenceDuration != nil || settings.SilenceRecovery != nil {
		h.encoder.UpdateSilenceConfig()
	}
	updateStringSetting(settings.SilenceWebhook, "webhook URL", h.cfg.SetWebhookURL)
	updateStringSetting(settings.SilenceLogPath, "log path", h.cfg.SetLogPath)
	if settings.GraphTenantID != nil || settings.GraphClientID != nil ||
		settings.GraphClientSecret != nil || settings.GraphFromAddress != nil ||
		settings.GraphRecipients != nil {
		// Get current values via snapshot (single mutex acquisition)
		snap := h.cfg.Snapshot()
		tenantID := snap.GraphTenantID
		clientID := snap.GraphClientID
		clientSecret := snap.GraphClientSecret
		fromAddress := snap.GraphFromAddress
		recipients := snap.GraphRecipients
		if settings.GraphTenantID != nil {
			tenantID = *settings.GraphTenantID
		}
		if settings.GraphClientID != nil {
			clientID = *settings.GraphClientID
		}
		if settings.GraphClientSecret != nil {
			clientSecret = *settings.GraphClientSecret
		}
		if settings.GraphFromAddress != nil {
			fromAddress = *settings.GraphFromAddress
		}
		if settings.GraphRecipients != nil {
			recipients = *settings.GraphRecipients
		}

		slog.Info("update_settings: updating Microsoft Graph configuration")
		if err := h.cfg.SetGraphConfig(tenantID, clientID, clientSecret, fromAddress, recipients); err != nil {
			slog.Error("update_settings: failed to save Graph config", "error", err)
		}
		// Notify encoder to update expiry checker with new config
		h.encoder.UpdateGraphConfig()
	}
	// Handle API key update
	if settings.RecordingAPIKey != nil {
		slog.Info("update_settings: updating recording API key")
		if err := h.cfg.SetRecordingAPIKey(*settings.RecordingAPIKey); err != nil {
			slog.Error("update_settings: failed to save API key", "error", err)
		}
	}
}

// handleRegenerateAPIKey processes a regenerate_api_key WebSocket command.
func (h *CommandHandler) handleRegenerateAPIKey(send chan<- interface{}) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in regenerate_api_key handler", "panic", r)
			}
		}()

		result := struct {
			Type   string `json:"type"`
			APIKey string `json:"api_key"`
			Error  string `json:"error,omitempty"`
		}{
			Type: "api_key_regenerated",
		}

		newKey, err := config.GenerateAPIKey()
		if err != nil {
			slog.Error("failed to generate API key", "error", err)
			result.Error = err.Error()
		} else {
			if err := h.cfg.SetRecordingAPIKey(newKey); err != nil {
				slog.Error("failed to save API key", "error", err)
				result.Error = err.Error()
			} else {
				result.APIKey = newKey
				slog.Info("API key regenerated")
			}
		}

		// Send via channel (non-blocking to prevent goroutine leak if channel is closed)
		select {
		case send <- result:
		default:
			slog.Warn("failed to send API key result: channel full or closed")
		}
	}()
}

// --- New slash-style command handlers ---

// sendResult sends a result message for a command
func sendResult(send chan<- interface{}, cmdType string, success bool, errMsg string, data interface{}) {
	result := map[string]interface{}{
		"type":    cmdType + "_result",
		"success": success,
	}
	if errMsg != "" {
		result["error"] = errMsg
	}
	if data != nil {
		result["data"] = data
	}
	select {
	case send <- result:
	default:
		slog.Warn("failed to send result: channel full or closed", "type", cmdType)
	}
}

// handleAudioUpdate handles audio/update command
func (h *CommandHandler) handleAudioUpdate(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		sendResult(send, "audio/update", false, err.Error(), nil)
		return
	}
	if data.Input != "" {
		h.handleAudioInputChange(data.Input)
	}
	sendResult(send, "audio/update", true, "", nil)
}

// handleAudioGet handles audio/get command
func (h *CommandHandler) handleAudioGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "audio/get", true, "", map[string]interface{}{
		"input": snap.AudioInput,
	})
}

// handleSilenceUpdate handles silence/update command
func (h *CommandHandler) handleSilenceUpdate(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		ThresholdDB *float64 `json:"threshold_db"`
		DurationMs  *int64   `json:"duration_ms"`
		RecoveryMs  *int64   `json:"recovery_ms"`
	}
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		sendResult(send, "silence/update", false, err.Error(), nil)
		return
	}

	if data.ThresholdDB != nil {
		if err := h.cfg.SetSilenceThreshold(*data.ThresholdDB); err != nil {
			sendResult(send, "silence/update", false, err.Error(), nil)
			return
		}
	}
	if data.DurationMs != nil {
		if err := h.cfg.SetSilenceDurationMs(*data.DurationMs); err != nil {
			sendResult(send, "silence/update", false, err.Error(), nil)
			return
		}
	}
	if data.RecoveryMs != nil {
		if err := h.cfg.SetSilenceRecoveryMs(*data.RecoveryMs); err != nil {
			sendResult(send, "silence/update", false, err.Error(), nil)
			return
		}
	}

	// Apply changes to encoder
	h.encoder.UpdateSilenceConfig()
	sendResult(send, "silence/update", true, "", nil)
}

// handleSilenceGet handles silence/get command
func (h *CommandHandler) handleSilenceGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "silence/get", true, "", map[string]interface{}{
		"threshold_db": snap.SilenceThreshold,
		"duration_ms":  snap.SilenceDurationMs,
		"recovery_ms":  snap.SilenceRecoveryMs,
	})
}

// handleWebhookUpdate handles notifications/webhook/update command
func (h *CommandHandler) handleWebhookUpdate(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		sendResult(send, "notifications/webhook/update", false, err.Error(), nil)
		return
	}
	if err := h.cfg.SetWebhookURL(data.URL); err != nil {
		sendResult(send, "notifications/webhook/update", false, err.Error(), nil)
		return
	}
	sendResult(send, "notifications/webhook/update", true, "", nil)
}

// handleWebhookGet handles notifications/webhook/get command
func (h *CommandHandler) handleWebhookGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "notifications/webhook/get", true, "", map[string]interface{}{
		"url": snap.WebhookURL,
	})
}

// handleLogUpdate handles notifications/log/update command
func (h *CommandHandler) handleLogUpdate(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		sendResult(send, "notifications/log/update", false, err.Error(), nil)
		return
	}
	if err := h.cfg.SetLogPath(data.Path); err != nil {
		sendResult(send, "notifications/log/update", false, err.Error(), nil)
		return
	}
	sendResult(send, "notifications/log/update", true, "", nil)
}

// handleLogGet handles notifications/log/get command
func (h *CommandHandler) handleLogGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "notifications/log/get", true, "", map[string]interface{}{
		"path": snap.LogPath,
	})
}

// handleEmailUpdate handles notifications/email/update command
func (h *CommandHandler) handleEmailUpdate(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		TenantID     string `json:"tenant_id"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		FromAddress  string `json:"from_address"`
		Recipients   string `json:"recipients"`
	}
	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		sendResult(send, "notifications/email/update", false, err.Error(), nil)
		return
	}
	if err := h.cfg.SetGraphConfig(data.TenantID, data.ClientID, data.ClientSecret, data.FromAddress, data.Recipients); err != nil {
		sendResult(send, "notifications/email/update", false, err.Error(), nil)
		return
	}
	h.encoder.UpdateGraphConfig()
	sendResult(send, "notifications/email/update", true, "", nil)
}

// handleEmailGet handles notifications/email/get command
func (h *CommandHandler) handleEmailGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "notifications/email/get", true, "", map[string]interface{}{
		"tenant_id":    snap.GraphTenantID,
		"client_id":    snap.GraphClientID,
		"from_address": snap.GraphFromAddress,
		"recipients":   snap.GraphRecipients,
		// Note: client_secret intentionally omitted for security
	})
}

// handleRecordingGet handles recording/get command
func (h *CommandHandler) handleRecordingGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	sendResult(send, "recording/get", true, "", map[string]interface{}{
		"api_key":              snap.RecordingAPIKey,
		"max_duration_minutes": snap.RecordingMaxDurationMinutes,
	})
}

// handleConfigGet handles config/get command - returns full config
func (h *CommandHandler) handleConfigGet(send chan<- interface{}) {
	snap := h.cfg.Snapshot()
	result := map[string]interface{}{
		"type": "config",
		"config": map[string]interface{}{
			"system": map[string]interface{}{
				"ffmpeg_path": snap.FFmpegPath,
				"port":        snap.WebPort,
				"username":    snap.WebUser,
				// password intentionally omitted
			},
			"web": map[string]interface{}{
				"station_name": snap.StationName,
				"color_light":  snap.StationColorLight,
				"color_dark":   snap.StationColorDark,
			},
			"audio": map[string]interface{}{
				"input": snap.AudioInput,
			},
			"silence_detection": map[string]interface{}{
				"threshold_db": snap.SilenceThreshold,
				"duration_ms":  snap.SilenceDurationMs,
				"recovery_ms":  snap.SilenceRecoveryMs,
			},
			"notifications": map[string]interface{}{
				"webhook": map[string]interface{}{
					"url": snap.WebhookURL,
				},
				"log": map[string]interface{}{
					"path": snap.LogPath,
				},
				"email": map[string]interface{}{
					"tenant_id":    snap.GraphTenantID,
					"client_id":    snap.GraphClientID,
					"from_address": snap.GraphFromAddress,
					"recipients":   snap.GraphRecipients,
				},
			},
			"streaming": map[string]interface{}{
				"outputs": snap.Outputs,
			},
			"recording": map[string]interface{}{
				"api_key":              snap.RecordingAPIKey,
				"max_duration_minutes": snap.RecordingMaxDurationMinutes,
				"recorders":            snap.Recorders,
			},
		},
	}
	select {
	case send <- result:
	default:
		slog.Warn("failed to send config: channel full or closed")
	}
}
