package server

import (
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// --- Audio handlers ---

// handleAudioUpdate processes an audio/update command.
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

// --- Silence detection handlers ---

// handleSilenceUpdate processes a silence/update command.
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

// --- Silence dump handlers ---

// handleSilenceDumpUpdate processes a silence_dump/update command.
func (h *CommandHandler) handleSilenceDumpUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *SilenceDumpUpdateRequest) error {
		snap := h.cfg.Snapshot()

		// Use current values as defaults if not provided
		enabled := snap.SilenceDumpEnabled
		retentionDays := snap.SilenceDumpRetentionDays

		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		if req.RetentionDays != nil {
			retentionDays = *req.RetentionDays
		}

		if err := h.cfg.SetSilenceDump(enabled, retentionDays); err != nil {
			return err
		}

		// Apply changes to encoder
		h.encoder.UpdateSilenceDumpConfig()
		return nil
	})
}

// --- Notification handlers ---

// handleWebhookUpdate processes a notifications/webhook/update command.
func (h *CommandHandler) handleWebhookUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *WebhookUpdateRequest) error {
		return h.cfg.SetWebhookURL(req.URL)
	})
}

// handleLogUpdate processes a notifications/log/update command.
func (h *CommandHandler) handleLogUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *LogUpdateRequest) error {
		return h.cfg.SetLogPath(req.Path)
	})
}

// handleEmailUpdate processes a notifications/email/update command.
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

// handleZabbixUpdate processes a notifications/zabbix/update command.
func (h *CommandHandler) handleZabbixUpdate(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *ZabbixUpdateRequest) error {
		return h.cfg.SetZabbixConfig(req.Server, req.Port, req.Host, req.Key)
	})
}

// --- Recording handlers ---

// handleRegenerateAPIKey processes a recording/regenerate-key command.
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

		return map[string]string{"api_key": newKey}, nil
	})
}
