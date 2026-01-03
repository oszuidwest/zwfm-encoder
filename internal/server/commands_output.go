package server

import (
	"log/slog"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// handleAddOutput processes an outputs/add command.
func (h *CommandHandler) handleAddOutput(cmd WSCommand, send chan<- any) {
	if !h.ffmpegAvailable {
		slog.Warn("outputs/add: FFmpeg not available")
		SendEntityResult(send, "output", "add", "", false, "FFmpeg not available")
		return
	}

	HandleCommand(h, cmd, send, func(req *OutputRequest) error {
		output := types.Output{
			Enabled:    true, // New outputs are enabled by default
			Host:       req.Host,
			Port:       req.Port,
			Password:   req.Password,
			StreamID:   req.StreamID,
			Codec:      types.Codec(req.Codec),
			MaxRetries: req.MaxRetries,
		}

		// Apply defaults
		if output.StreamID == "" {
			output.StreamID = "studio"
		}
		if output.Codec == "" {
			output.Codec = types.CodecWAV
		}

		// Check output limit
		if len(h.cfg.ConfiguredOutputs()) >= MaxOutputs {
			slog.Warn("outputs/add: maximum outputs reached", "max", MaxOutputs)
			SendEntityResult(send, "output", "add", "", false, "maximum outputs reached")
			return nil
		}

		if err := h.cfg.AddOutput(&output); err != nil {
			slog.Error("outputs/add: failed to add", "error", err)
			SendEntityResult(send, "output", "add", "", false, err.Error())
			return nil
		}

		slog.Info("outputs/add: added output", "host", output.Host, "port", output.Port)

		// Start output if encoder is running
		if h.encoder.State() == types.StateRunning {
			outputs := h.cfg.ConfiguredOutputs()
			if len(outputs) > 0 {
				if err := h.encoder.StartOutput(outputs[len(outputs)-1].ID); err != nil {
					slog.Error("outputs/add: failed to start output", "error", err)
				}
			}
		}

		SendEntityResult(send, "output", "add", output.ID, true, "")
		return nil
	})
}

// handleDeleteOutput processes an outputs/delete command.
func (h *CommandHandler) handleDeleteOutput(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("outputs/delete: no ID provided")
		SendEntityResult(send, "output", "delete", "", false, "no ID provided")
		return
	}

	slog.Info("outputs/delete: deleting", "output_id", cmd.ID)
	if err := h.encoder.StopOutput(cmd.ID); err != nil {
		slog.Error("outputs/delete: failed to stop", "error", err)
	}

	if err := h.cfg.RemoveOutput(cmd.ID); err != nil {
		slog.Error("outputs/delete: failed to remove from config", "error", err)
		SendEntityResult(send, "output", "delete", cmd.ID, false, err.Error())
		return
	}

	slog.Info("outputs/delete: removed from config", "output_id", cmd.ID)
	SendEntityResult(send, "output", "delete", cmd.ID, true, "")
}

// handleUpdateOutput processes an outputs/update command.
func (h *CommandHandler) handleUpdateOutput(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("outputs/update: no ID provided")
		SendEntityResult(send, "output", "update", "", false, "no ID provided")
		return
	}

	existing := h.cfg.Output(cmd.ID)
	if existing == nil {
		slog.Warn("outputs/update: output not found", "output_id", cmd.ID)
		SendEntityResult(send, "output", "update", cmd.ID, false, "output not found")
		return
	}

	HandleCommand(h, cmd, send, func(req *OutputRequest) error {
		updated := types.Output{
			ID:         existing.ID,
			CreatedAt:  existing.CreatedAt,
			Enabled:    req.Enabled,
			Host:       req.Host,
			Port:       req.Port,
			Password:   req.Password,
			StreamID:   req.StreamID,
			Codec:      types.Codec(req.Codec),
			MaxRetries: req.MaxRetries,
		}

		// Apply defaults
		if updated.StreamID == "" {
			updated.StreamID = "studio"
		}
		if updated.Codec == "" {
			updated.Codec = types.CodecWAV
		}

		// Preserve password if not provided
		if updated.Password == "" {
			updated.Password = existing.Password
		}

		wasEnabled := existing.IsEnabled()
		nowEnabled := updated.IsEnabled()
		needsRestart := outputNeedsRestart(existing, &updated)

		if err := h.cfg.UpdateOutput(&updated); err != nil {
			slog.Error("outputs/update: failed to update", "error", err)
			SendEntityResult(send, "output", "update", cmd.ID, false, err.Error())
			return nil
		}

		slog.Info("outputs/update: updated output", "output_id", updated.ID, "host", updated.Host, "port", updated.Port)

		// Handle output state changes when encoder is running
		if h.encoder.State() == types.StateRunning {
			switch {
			case wasEnabled && !nowEnabled:
				slog.Info("outputs/update: stopping disabled output", "output_id", updated.ID)
				if err := h.encoder.StopOutput(updated.ID); err != nil {
					slog.Error("outputs/update: failed to stop", "output_id", updated.ID, "error", err)
				}
			case !wasEnabled && nowEnabled:
				slog.Info("outputs/update: starting enabled output", "output_id", updated.ID)
				if err := h.encoder.StartOutput(updated.ID); err != nil {
					slog.Error("outputs/update: failed to start", "output_id", updated.ID, "error", err)
				}
			case nowEnabled && needsRestart:
				slog.Info("outputs/update: restarting output due to connection changes", "output_id", updated.ID)
				h.restartOutput(updated.ID)
			}
		}

		SendEntityResult(send, "output", "update", updated.ID, true, "")
		return nil
	})
}

// outputNeedsRestart reports whether an output needs restart after configuration changes.
func outputNeedsRestart(existing, updated *types.Output) bool {
	return existing.Host != updated.Host ||
		existing.Port != updated.Port ||
		existing.Password != updated.Password ||
		existing.StreamID != updated.StreamID ||
		existing.Codec != updated.Codec
}

// restartOutput stops and restarts an output after a brief delay.
func (h *CommandHandler) restartOutput(outputID string) {
	if err := h.encoder.StopOutput(outputID); err != nil {
		slog.Error("restartOutput: failed to stop", "output_id", outputID, "error", err)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in restartOutput", "output_id", outputID, "panic", r)
			}
		}()
		time.Sleep(types.OutputRestartDelay)
		if h.encoder.State() != types.StateRunning {
			return
		}
		if err := h.encoder.StartOutput(outputID); err != nil {
			slog.Error("restartOutput: failed to start", "output_id", outputID, "error", err)
		}
	}()
}
