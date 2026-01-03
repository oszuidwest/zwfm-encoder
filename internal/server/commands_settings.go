package server

import (
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// --- Audio handlers ---

// handleAudioUpdate handles audio/update command.
func (h *CommandHandler) handleAudioUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *AudioUpdateRequest) error {
		if req.Input == "" {
			return nil // No change requested
		}

		slog.Info("audio/update: changing audio input", "input", req.Input)
		if err := h.cfg.SetAudioInput(req.Input); err != nil {
			return err
		}

		// Start/restart encoder if FFmpeg is available
		if h.ffmpegAvailable {
			go func() {
				var err error
				switch h.encoder.State() {
				case types.StateRunning:
					err = h.encoder.Restart()
				case types.StateStopped:
					err = h.encoder.Start()
				}
				if err != nil {
					slog.Error("audio/update: encoder state change failed", "error", err)
				}
			}()
		}

		return nil
	})
}

// handleAudioGet handles audio/get command.
func (h *CommandHandler) handleAudioGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "audio/get", map[string]any{
		"input": snap.AudioInput,
	})
}

// --- Silence detection handlers ---

// handleSilenceUpdate handles silence/update command.
func (h *CommandHandler) handleSilenceUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *SilenceUpdateRequest) error {
		if req.ThresholdDB != nil {
			if err := h.cfg.SetSilenceThreshold(*req.ThresholdDB); err != nil {
				return err
			}
		}
		if req.DurationMs != nil {
			if err := h.cfg.SetSilenceDurationMs(*req.DurationMs); err != nil {
				return err
			}
		}
		if req.RecoveryMs != nil {
			if err := h.cfg.SetSilenceRecoveryMs(*req.RecoveryMs); err != nil {
				return err
			}
		}

		// Apply changes to encoder
		h.encoder.UpdateSilenceConfig()
		return nil
	})
}

// handleSilenceGet handles silence/get command.
func (h *CommandHandler) handleSilenceGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "silence/get", map[string]any{
		"threshold_db": snap.SilenceThreshold,
		"duration_ms":  snap.SilenceDurationMs,
		"recovery_ms":  snap.SilenceRecoveryMs,
	})
}

// --- Notification handlers ---

// handleWebhookUpdate handles notifications/webhook/update command.
func (h *CommandHandler) handleWebhookUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *WebhookUpdateRequest) error {
		return h.cfg.SetWebhookURL(req.URL)
	})
}

// handleWebhookGet handles notifications/webhook/get command.
func (h *CommandHandler) handleWebhookGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "notifications/webhook/get", map[string]any{
		"url": snap.WebhookURL,
	})
}

// handleLogUpdate handles notifications/log/update command.
func (h *CommandHandler) handleLogUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *LogUpdateRequest) error {
		return h.cfg.SetLogPath(req.Path)
	})
}

// handleLogGet handles notifications/log/get command.
func (h *CommandHandler) handleLogGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "notifications/log/get", map[string]any{
		"path": snap.LogPath,
	})
}

// handleEmailUpdate handles notifications/email/update command.
func (h *CommandHandler) handleEmailUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *EmailUpdateRequest) error {
		if err := h.cfg.SetGraphConfig(
			req.TenantID,
			req.ClientID,
			req.ClientSecret,
			req.FromAddress,
			req.Recipients,
		); err != nil {
			return err
		}
		h.encoder.UpdateGraphConfig()
		return nil
	})
}

// handleEmailGet handles notifications/email/get command.
func (h *CommandHandler) handleEmailGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "notifications/email/get", map[string]any{
		"tenant_id":    snap.GraphTenantID,
		"client_id":    snap.GraphClientID,
		"from_address": snap.GraphFromAddress,
		"recipients":   snap.GraphRecipients,
		// Note: client_secret intentionally omitted for security
	})
}

// --- Recording handlers ---

// handleRecordingGet handles recording/get command.
func (h *CommandHandler) handleRecordingGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	SendSuccess(send, "recording/get", map[string]any{
		"api_key":              snap.RecordingAPIKey,
		"max_duration_minutes": snap.RecordingMaxDurationMinutes,
	})
}

// handleRegenerateAPIKey handles recording/regenerate-key command.
func (h *CommandHandler) handleRegenerateAPIKey(send chan<- any) {
	HandleActionAsync(WSCommand{Type: "recording/regenerate-key"}, send, func() (any, error) {
		newKey, err := config.GenerateAPIKey()
		if err != nil {
			return nil, err
		}

		if err := h.cfg.SetRecordingAPIKey(newKey); err != nil {
			return nil, err
		}

		slog.Info("API key regenerated")

		// Return custom response format for backwards compatibility
		return map[string]string{"api_key": newKey}, nil
	})
}

// --- Config handlers ---

// handleConfigGet handles config/get command - returns full config.
func (h *CommandHandler) handleConfigGet(send chan<- any) {
	snap := h.cfg.Snapshot()
	result := map[string]any{
		"type": "config",
		"config": map[string]any{
			"system": map[string]any{
				"ffmpeg_path": snap.FFmpegPath,
				"port":        snap.WebPort,
				"username":    snap.WebUser,
				// password intentionally omitted
			},
			"web": map[string]any{
				"station_name": snap.StationName,
				"color_light":  snap.StationColorLight,
				"color_dark":   snap.StationColorDark,
			},
			"audio": map[string]any{
				"input": snap.AudioInput,
			},
			"silence_detection": map[string]any{
				"threshold_db": snap.SilenceThreshold,
				"duration_ms":  snap.SilenceDurationMs,
				"recovery_ms":  snap.SilenceRecoveryMs,
			},
			"notifications": map[string]any{
				"webhook": map[string]any{
					"url": snap.WebhookURL,
				},
				"log": map[string]any{
					"path": snap.LogPath,
				},
				"email": map[string]any{
					"tenant_id":    snap.GraphTenantID,
					"client_id":    snap.GraphClientID,
					"from_address": snap.GraphFromAddress,
					"recipients":   snap.GraphRecipients,
				},
			},
			"streaming": map[string]any{
				"outputs": snap.Outputs,
			},
			"recording": map[string]any{
				"api_key":              snap.RecordingAPIKey,
				"max_duration_minutes": snap.RecordingMaxDurationMinutes,
				"recorders":            snap.Recorders,
			},
		},
	}
	SendData(send, result)
}
