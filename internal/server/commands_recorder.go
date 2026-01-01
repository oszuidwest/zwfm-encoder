package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// validateLocalPath checks that LocalPath is set, validates it for security,
// and creates the directory.
func validateLocalPath(recorder *types.Recorder) error {
	// Security: validate path to prevent path traversal attacks
	if err := util.ValidatePath("local_path", recorder.LocalPath); err != nil {
		return err
	}
	if err := os.MkdirAll(recorder.LocalPath, 0o755); err != nil {
		return fmt.Errorf("cannot create local_path directory: %w", err)
	}
	return nil
}

// validateS3Fields checks that required S3 fields are set.
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

// validateRecorder validates a recorder configuration.
func validateRecorder(recorder *types.Recorder) error {
	if err := util.ValidateRequired("name", recorder.Name); err != nil {
		return err
	}

	// Validate rotation mode
	if recorder.RotationMode != types.RotationHourly && recorder.RotationMode != types.RotationOnDemand {
		return fmt.Errorf("invalid rotation_mode: must be 'hourly' or 'ondemand'")
	}

	// Validate storage mode and required fields
	switch recorder.StorageMode {
	case types.StorageLocal:
		if err := validateLocalPath(recorder); err != nil {
			return err
		}
	case types.StorageS3:
		if err := validateS3Fields(recorder); err != nil {
			return err
		}
	case types.StorageBoth:
		if err := validateLocalPath(recorder); err != nil {
			return err
		}
		if err := validateS3Fields(recorder); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid storage_mode: must be 'local', 's3', or 'both'")
	}

	// Apply defaults
	if recorder.Codec == "" {
		recorder.Codec = types.DefaultCodec
	}
	if recorder.RetentionDays == 0 {
		recorder.RetentionDays = types.DefaultRetentionDays
	}

	return nil
}

// handleAddRecorder creates a new recorder.
func (h *CommandHandler) handleAddRecorder(cmd WSCommand, send chan<- interface{}) {
	var recorder types.Recorder
	if err := json.Unmarshal(cmd.Data, &recorder); err != nil {
		slog.Warn("add_recorder: invalid JSON data", "error", err)
		sendRecorderResult(send, "add", "", false, err.Error())
		return
	}

	if err := validateRecorder(&recorder); err != nil {
		slog.Warn("add_recorder: validation failed", "error", err)
		sendRecorderResult(send, "add", "", false, err.Error())
		return
	}

	if err := h.encoder.AddRecorder(&recorder); err != nil {
		slog.Error("add_recorder: failed to add", "error", err)
		sendRecorderResult(send, "add", "", false, err.Error())
		return
	}

	slog.Info("add_recorder: added recorder", "id", recorder.ID, "name", recorder.Name)
	sendRecorderResult(send, "add", recorder.ID, true, "")
}

// handleDeleteRecorder removes a recorder.
func (h *CommandHandler) handleDeleteRecorder(cmd WSCommand, send chan<- interface{}) {
	if cmd.ID == "" {
		slog.Warn("delete_recorder: no ID provided")
		sendRecorderResult(send, "delete", "", false, "no ID provided")
		return
	}

	if err := h.encoder.RemoveRecorder(cmd.ID); err != nil {
		slog.Error("delete_recorder: failed to remove", "error", err)
		sendRecorderResult(send, "delete", cmd.ID, false, err.Error())
		return
	}

	slog.Info("delete_recorder: removed recorder", "id", cmd.ID)
	sendRecorderResult(send, "delete", cmd.ID, true, "")
}

// handleUpdateRecorder updates a recorder configuration.
func (h *CommandHandler) handleUpdateRecorder(cmd WSCommand, send chan<- interface{}) {
	if cmd.ID == "" {
		slog.Warn("update_recorder: no ID provided")
		sendRecorderResult(send, "update", "", false, "no ID provided")
		return
	}

	existing := h.cfg.Recorder(cmd.ID)
	if existing == nil {
		slog.Warn("update_recorder: recorder not found", "id", cmd.ID)
		sendRecorderResult(send, "update", cmd.ID, false, "recorder not found")
		return
	}

	var updated types.Recorder
	if err := json.Unmarshal(cmd.Data, &updated); err != nil {
		slog.Warn("update_recorder: invalid JSON data", "error", err)
		sendRecorderResult(send, "update", cmd.ID, false, err.Error())
		return
	}

	// Preserve immutable fields and secret if not provided (before validation)
	updated.ID = existing.ID
	updated.CreatedAt = existing.CreatedAt
	if updated.S3SecretAccessKey == "" {
		updated.S3SecretAccessKey = existing.S3SecretAccessKey
	}

	if err := validateRecorder(&updated); err != nil {
		slog.Warn("update_recorder: validation failed", "error", err)
		sendRecorderResult(send, "update", cmd.ID, false, err.Error())
		return
	}

	if err := h.encoder.UpdateRecorder(&updated); err != nil {
		slog.Error("update_recorder: failed to update", "error", err)
		sendRecorderResult(send, "update", cmd.ID, false, err.Error())
		return
	}

	slog.Info("update_recorder: updated recorder", "id", updated.ID, "name", updated.Name)
	sendRecorderResult(send, "update", updated.ID, true, "")
}

// handleStartRecorder starts a specific recorder.
func (h *CommandHandler) handleStartRecorder(cmd WSCommand, send chan<- interface{}) {
	if cmd.ID == "" {
		slog.Warn("start_recorder: no ID provided")
		sendRecorderResult(send, "start", "", false, "no ID provided")
		return
	}

	if err := h.encoder.StartRecorder(cmd.ID); err != nil {
		slog.Error("start_recorder: failed to start", "error", err)
		sendRecorderResult(send, "start", cmd.ID, false, err.Error())
		return
	}

	slog.Info("start_recorder: started recorder", "id", cmd.ID)
	sendRecorderResult(send, "start", cmd.ID, true, "")
}

// handleStopRecorder stops a specific recorder.
func (h *CommandHandler) handleStopRecorder(cmd WSCommand, send chan<- interface{}) {
	if cmd.ID == "" {
		slog.Warn("stop_recorder: no ID provided")
		sendRecorderResult(send, "stop", "", false, "no ID provided")
		return
	}

	if err := h.encoder.StopRecorder(cmd.ID); err != nil {
		slog.Error("stop_recorder: failed to stop", "error", err)
		sendRecorderResult(send, "stop", cmd.ID, false, err.Error())
		return
	}

	slog.Info("stop_recorder: stopped recorder", "id", cmd.ID)
	sendRecorderResult(send, "stop", cmd.ID, true, "")
}

// handleTestRecorderS3 tests S3 connectivity for a recorder.
func (h *CommandHandler) handleTestRecorderS3(cmd WSCommand, send chan<- interface{}) {
	var data struct {
		Endpoint  string `json:"s3_endpoint"`
		Bucket    string `json:"s3_bucket"`
		AccessKey string `json:"s3_access_key_id"`
		SecretKey string `json:"s3_secret_access_key"`
	}

	if err := json.Unmarshal(cmd.Data, &data); err != nil {
		slog.Warn("test_recorder_s3: invalid JSON data", "error", err)
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in test_recorder_s3 handler", "panic", r)
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

		cfg := &types.Recorder{
			S3Endpoint:        data.Endpoint,
			S3Bucket:          data.Bucket,
			S3AccessKeyID:     data.AccessKey,
			S3SecretAccessKey: data.SecretKey,
		}

		if err := recording.TestRecorderS3Connection(cfg); err != nil {
			slog.Error("recorder S3 connection test failed", "error", err)
			result.Success = false
			result.Error = err.Error()
		} else {
			slog.Info("recorder S3 connection test succeeded")
		}

		// Send via channel (non-blocking to prevent goroutine leak if channel is closed)
		select {
		case send <- result:
		default:
			slog.Warn("failed to send S3 test response: channel full or closed")
		}
	}()
}

// sendRecorderResult sends a recorder operation result to the client.
func sendRecorderResult(send chan<- interface{}, action, id string, success bool, errMsg string) {
	result := struct {
		Type    string `json:"type"`
		Action  string `json:"action"`
		ID      string `json:"id,omitempty"`
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}{
		Type:    "recorder_result",
		Action:  action,
		ID:      id,
		Success: success,
		Error:   errMsg,
	}

	// Send via channel (non-blocking to prevent goroutine leak if channel is closed)
	select {
	case send <- result:
	default:
		slog.Warn("failed to send recorder result: channel full or closed", "action", action)
	}
}
