package server

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// validateOutput validates and applies defaults to an output configuration.
func validateOutput(output *types.Output) error {
	if err := util.ValidateRequired("host", output.Host); err != nil {
		return err
	}
	if err := util.ValidatePort("port", output.Port); err != nil {
		return err
	}
	if err := util.ValidateMaxLength("host", output.Host, MaxHostLength); err != nil {
		return err
	}
	if err := util.ValidateMaxLength("streamid", output.StreamID, MaxStreamIDLength); err != nil {
		return err
	}
	if err := util.ValidateRange("max_retries", output.MaxRetries, 0, MaxRetriesLimit); err != nil {
		return err
	}
	// Apply defaults
	if output.StreamID == "" {
		output.StreamID = "studio"
	}
	if output.Codec == "" {
		output.Codec = types.DefaultCodec
	}
	return nil
}

func (h *CommandHandler) handleAddOutput(cmd WSCommand) {
	if !h.ffmpegAvailable {
		slog.Warn("add_output: FFmpeg not available, cannot add output")
		return
	}
	var output types.Output
	if err := json.Unmarshal(cmd.Data, &output); err != nil {
		slog.Warn("add_output: invalid JSON data", "error", err)
		return
	}
	if err := validateOutput(&output); err != nil {
		slog.Warn("add_output: validation failed", "error", err)
		return
	}
	// Limit number of outputs to prevent resource exhaustion
	if len(h.cfg.ConfiguredOutputs()) >= MaxOutputs {
		slog.Warn("add_output: maximum outputs reached", "max", MaxOutputs)
		return
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

// outputNeedsRestart checks if connection parameters changed between existing and updated output.
func outputNeedsRestart(existing, updated *types.Output) bool {
	return existing.Host != updated.Host ||
		existing.Port != updated.Port ||
		existing.Password != updated.Password ||
		existing.StreamID != updated.StreamID ||
		existing.Codec != updated.Codec
}

func (h *CommandHandler) handleUpdateOutput(cmd WSCommand) {
	if cmd.ID == "" {
		slog.Warn("update_output: no ID provided")
		return
	}
	existing := h.cfg.Output(cmd.ID)
	if existing == nil {
		slog.Warn("update_output: output not found", "output_id", cmd.ID)
		return
	}

	var updated types.Output
	if err := json.Unmarshal(cmd.Data, &updated); err != nil {
		slog.Warn("update_output: invalid JSON data", "error", err)
		return
	}
	if err := validateOutput(&updated); err != nil {
		slog.Warn("update_output: validation failed", "error", err)
		return
	}

	// Preserve immutable fields and password if not provided
	updated.ID = existing.ID
	updated.CreatedAt = existing.CreatedAt
	if updated.Password == "" {
		updated.Password = existing.Password
	}

	wasEnabled := existing.IsEnabled()
	nowEnabled := updated.IsEnabled()
	needsRestart := outputNeedsRestart(existing, &updated)

	if err := h.cfg.UpdateOutput(&updated); err != nil {
		slog.Error("update_output: failed to update", "error", err)
		return
	}
	slog.Info("update_output: updated output", "output_id", updated.ID, "host", updated.Host, "port", updated.Port)

	// Clear any error/given_up state so output can retry with new config
	h.encoder.ClearOutputErrorState(updated.ID)

	// Handle output state changes when encoder is running
	if h.encoder.State() == types.StateRunning {
		switch {
		case wasEnabled && !nowEnabled:
			// Output was disabled - stop it
			slog.Info("update_output: stopping disabled output", "output_id", updated.ID)
			if err := h.encoder.StopOutput(updated.ID); err != nil {
				slog.Error("update_output: failed to stop", "output_id", updated.ID, "error", err)
			}
		case !wasEnabled && nowEnabled:
			// Output was enabled - start it
			slog.Info("update_output: starting enabled output", "output_id", updated.ID)
			if err := h.encoder.StartOutput(updated.ID); err != nil {
				slog.Error("update_output: failed to start", "output_id", updated.ID, "error", err)
			}
		case nowEnabled && needsRestart:
			// Connection params changed on enabled output - restart it
			slog.Info("update_output: restarting output due to connection changes", "output_id", updated.ID)
			h.restartOutput(updated.ID)
		}
	}
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
			return // Encoder stopped while waiting
		}
		if err := h.encoder.StartOutput(outputID); err != nil {
			slog.Error("restartOutput: failed to start", "output_id", outputID, "error", err)
		}
	}()
}
