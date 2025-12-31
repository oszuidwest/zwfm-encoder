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

// handleRegenerateAPIKey generates a new API key.
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
