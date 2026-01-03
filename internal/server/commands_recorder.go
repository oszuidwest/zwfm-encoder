package server

import (
	"fmt"
	"log/slog"

	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// handleAddRecorder processes a recorders/add command.
func (h *CommandHandler) handleAddRecorder(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *RecorderRequest) error {
		codec, _ := types.ParseCodec(req.Codec)
		rotationMode, _ := types.ParseRotationMode(req.RotationMode)
		storageMode, _ := types.ParseStorageMode(req.StorageMode)

		recorder := types.Recorder{
			Name:              req.Name,
			Enabled:           true, // New recorders are enabled by default
			Codec:             codec,
			RotationMode:      rotationMode,
			StorageMode:       storageMode,
			LocalPath:         req.LocalPath,
			S3Endpoint:        req.S3Endpoint,
			S3Bucket:          req.S3Bucket,
			S3AccessKeyID:     req.S3AccessKeyID,
			S3SecretAccessKey: req.S3SecretAccessKey,
			RetentionDays:     req.RetentionDays,
		}

		// Apply defaults
		if !recorder.Codec.IsValid() {
			recorder.Codec = types.CodecWAV
		}
		if recorder.RetentionDays == 0 {
			recorder.RetentionDays = types.DefaultRetentionDays
		}

		// Validate storage-specific requirements
		if err := validateRecorderStorage(&recorder); err != nil {
			SendEntityResult(send, "recorder", "add", "", false, err.Error())
			return nil
		}

		if err := h.encoder.AddRecorder(&recorder); err != nil {
			slog.Error("recorders/add: failed to add", "error", err)
			SendEntityResult(send, "recorder", "add", "", false, err.Error())
			return nil
		}

		slog.Info("recorders/add: added recorder", "id", recorder.ID, "name", recorder.Name)
		SendEntityResult(send, "recorder", "add", recorder.ID, true, "")
		return nil
	})
}

// handleDeleteRecorder processes a recorders/delete command.
func (h *CommandHandler) handleDeleteRecorder(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("recorders/delete: no ID provided")
		SendEntityResult(send, "recorder", "delete", "", false, "no ID provided")
		return
	}

	if err := h.encoder.RemoveRecorder(cmd.ID); err != nil {
		slog.Error("recorders/delete: failed to remove", "error", err)
		SendEntityResult(send, "recorder", "delete", cmd.ID, false, err.Error())
		return
	}

	slog.Info("recorders/delete: removed recorder", "id", cmd.ID)
	SendEntityResult(send, "recorder", "delete", cmd.ID, true, "")
}

// handleUpdateRecorder processes a recorders/update command.
func (h *CommandHandler) handleUpdateRecorder(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("recorders/update: no ID provided")
		SendEntityResult(send, "recorder", "update", "", false, "no ID provided")
		return
	}

	existing := h.cfg.Recorder(cmd.ID)
	if existing == nil {
		slog.Warn("recorders/update: recorder not found", "id", cmd.ID)
		SendEntityResult(send, "recorder", "update", cmd.ID, false, "recorder not found")
		return
	}

	HandleCommand(h, cmd, send, func(req *RecorderRequest) error {
		codec, _ := types.ParseCodec(req.Codec)
		rotationMode, _ := types.ParseRotationMode(req.RotationMode)
		storageMode, _ := types.ParseStorageMode(req.StorageMode)

		updated := types.Recorder{
			ID:                existing.ID,
			CreatedAt:         existing.CreatedAt,
			Name:              req.Name,
			Enabled:           req.Enabled,
			Codec:             codec,
			RotationMode:      rotationMode,
			StorageMode:       storageMode,
			LocalPath:         req.LocalPath,
			S3Endpoint:        req.S3Endpoint,
			S3Bucket:          req.S3Bucket,
			S3AccessKeyID:     req.S3AccessKeyID,
			S3SecretAccessKey: req.S3SecretAccessKey,
			RetentionDays:     req.RetentionDays,
		}

		// Apply defaults
		if !updated.Codec.IsValid() {
			updated.Codec = types.CodecWAV
		}
		if updated.RetentionDays == 0 {
			updated.RetentionDays = types.DefaultRetentionDays
		}

		// Preserve secret if not provided
		if updated.S3SecretAccessKey == "" {
			updated.S3SecretAccessKey = existing.S3SecretAccessKey
		}

		// Validate storage-specific requirements
		if err := validateRecorderStorage(&updated); err != nil {
			SendEntityResult(send, "recorder", "update", cmd.ID, false, err.Error())
			return nil
		}

		if err := h.encoder.UpdateRecorder(&updated); err != nil {
			slog.Error("recorders/update: failed to update", "error", err)
			SendEntityResult(send, "recorder", "update", cmd.ID, false, err.Error())
			return nil
		}

		slog.Info("recorders/update: updated recorder", "id", updated.ID, "name", updated.Name)
		SendEntityResult(send, "recorder", "update", updated.ID, true, "")
		return nil
	})
}

// handleStartRecorder processes a recorders/start command.
func (h *CommandHandler) handleStartRecorder(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("recorders/start: no ID provided")
		SendEntityResult(send, "recorder", "start", "", false, "no ID provided")
		return
	}

	if err := h.encoder.StartRecorder(cmd.ID); err != nil {
		slog.Error("recorders/start: failed to start", "error", err)
		SendEntityResult(send, "recorder", "start", cmd.ID, false, err.Error())
		return
	}

	slog.Info("recorders/start: started recorder", "id", cmd.ID)
	SendEntityResult(send, "recorder", "start", cmd.ID, true, "")
}

// handleStopRecorder processes a recorders/stop command.
func (h *CommandHandler) handleStopRecorder(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("recorders/stop: no ID provided")
		SendEntityResult(send, "recorder", "stop", "", false, "no ID provided")
		return
	}

	if err := h.encoder.StopRecorder(cmd.ID); err != nil {
		slog.Error("recorders/stop: failed to stop", "error", err)
		SendEntityResult(send, "recorder", "stop", cmd.ID, false, err.Error())
		return
	}

	slog.Info("recorders/stop: stopped recorder", "id", cmd.ID)
	SendEntityResult(send, "recorder", "stop", cmd.ID, true, "")
}

// handleClearRecorderError processes a recorders/clear-error command.
func (h *CommandHandler) handleClearRecorderError(cmd WSCommand, send chan<- any) {
	if cmd.ID == "" {
		slog.Warn("recorders/clear-error: no ID provided")
		SendEntityResult(send, "recorder", "clear_error", "", false, "no ID provided")
		return
	}

	if err := h.encoder.ClearRecorderError(cmd.ID); err != nil {
		slog.Error("recorders/clear-error: failed to clear", "error", err)
		SendEntityResult(send, "recorder", "clear_error", cmd.ID, false, err.Error())
		return
	}

	slog.Info("recorders/clear-error: cleared error", "id", cmd.ID)
	SendEntityResult(send, "recorder", "clear_error", cmd.ID, true, "")
}

// handleTestRecorderS3 processes a recorders/test-s3 command.
func (h *CommandHandler) handleTestRecorderS3(cmd WSCommand, send chan<- any) {
	HandleCommand(h, cmd, send, func(req *S3TestRequest) error {
		cfg := &types.Recorder{
			S3Endpoint:        req.Endpoint,
			S3Bucket:          req.Bucket,
			S3AccessKeyID:     req.AccessKey,
			S3SecretAccessKey: req.SecretKey,
		}

		// Run S3 test async
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in recorders/test-s3 handler", "panic", r)
				}
			}()

			result := struct {
				Type    string `json:"type"`
				Success bool   `json:"success"`
				Error   string `json:"error,omitempty"`
			}{
				Type:    "recorder_s3_test_result",
				Success: true,
			}

			if err := recording.TestRecorderS3Connection(cfg); err != nil {
				slog.Error("recorders/test-s3: connection test failed", "error", err)
				result.Success = false
				result.Error = err.Error()
			} else {
				slog.Info("recorders/test-s3: connection test succeeded")
			}

			SendData(send, result)
		}()

		return nil
	})
}

// validateRecorderStorage validates storage-specific requirements.
func validateRecorderStorage(recorder *types.Recorder) error {
	switch recorder.StorageMode {
	case types.StorageLocal:
		return validateLocalPath(recorder)
	case types.StorageS3:
		return validateS3Fields(recorder)
	case types.StorageBoth:
		if err := validateLocalPath(recorder); err != nil {
			return err
		}
		return validateS3Fields(recorder)
	default:
		return fmt.Errorf("invalid storage_mode: must be 'local', 's3', or 'both'")
	}
}

// validateLocalPath validates the recorder's local path.
func validateLocalPath(recorder *types.Recorder) error {
	if err := util.ValidatePath("local_path", recorder.LocalPath); err != nil {
		return err
	}
	if err := util.CheckPathWritable(recorder.LocalPath); err != nil {
		return err
	}
	return nil
}

// validateS3Fields validates required S3 fields.
func validateS3Fields(recorder *types.Recorder) error {
	if recorder.S3Bucket == "" {
		return fmt.Errorf("s3_bucket is required")
	}
	if recorder.S3AccessKeyID == "" {
		return fmt.Errorf("s3_access_key_id is required")
	}
	if recorder.S3SecretAccessKey == "" {
		return fmt.Errorf("s3_secret_access_key is required")
	}
	return nil
}
