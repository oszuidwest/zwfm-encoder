package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) writeMessage(w http.ResponseWriter, message string) {
	s.writeJSON(w, http.StatusOK, map[string]string{"message": message})
}

func (s *Server) writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeConfigError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrStreamNotFound) || errors.Is(err, config.ErrRecorderNotFound) {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, config.ErrInvalidStreamConfig) {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, encoder.ErrRecordingNotAvailable) {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.writeError(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// maxRequestBodySize limits JSON request bodies to 1MB.
const maxRequestBodySize = 1 << 20

// parseJSON parses JSON from the request body into type T and reports whether parsing succeeded.
// Limits request body size to prevent denial of service attacks.
func parseJSON[T any](s *Server, w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	if err := s.readJSON(r, &v); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return v, false
	}
	return v, true
}

// redactSlice maps each element through redact, taking the address so large
// structs are not copied by value.
func redactSlice[T, R any](items []T, redact func(*T) R) []R {
	resp := make([]R, len(items))
	for i := range items {
		resp[i] = redact(&items[i])
	}
	return resp
}

func redactStream(stream *types.Stream) types.StreamResponse {
	return types.StreamResponse{
		ID:          stream.ID,
		Enabled:     stream.Enabled,
		Mode:        stream.ModeOrDefault(),
		Host:        stream.Host,
		Port:        stream.Port,
		HasPassword: stream.Password != "",
		StreamID:    stream.StreamID,
		Codec:       stream.Codec,
		Bitrate:     stream.Bitrate,
		MaxRetries:  stream.MaxRetries,
		CreatedAt:   stream.CreatedAt,
	}
}

func redactRecorder(recorder *types.Recorder) types.RecorderResponse {
	return types.RecorderResponse{
		ID:            recorder.ID,
		Name:          recorder.Name,
		Enabled:       recorder.Enabled,
		Codec:         recorder.Codec,
		Bitrate:       recorder.Bitrate,
		RecordingMode: recorder.RecordingMode,
		StorageMode:   recorder.StorageMode,
		LocalPath:     recorder.LocalPath,
		S3Endpoint:    recorder.S3Endpoint,
		S3Bucket:      recorder.S3Bucket,
		S3AccessKeyID: recorder.S3AccessKeyID,
		HasS3Secret:   recorder.S3SecretAccessKey != "",
		RetentionDays: recorder.RetentionDays,
		CreatedAt:     recorder.CreatedAt,
	}
}

func validateRecorderLocalPath(recorder *types.Recorder) error {
	if recorder.StorageMode == types.StorageS3 {
		return nil
	}
	if err := util.ValidatePath("local_path", recorder.LocalPath); err != nil {
		return err
	}
	if err := util.CheckPathWritable(recorder.LocalPath); err != nil {
		return fmt.Errorf("local_path: %w", err)
	}
	return nil
}

// handleAPIConfig returns the full configuration for the frontend.
func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()

	resp := types.APIConfigResponse{
		// Audio
		AudioInput: cfg.AudioInput,
		Devices:    audio.Devices(),
		Platform:   runtime.GOOS,

		// Silence detection
		SilenceThreshold:  cfg.SilenceThreshold,
		SilenceDurationMs: cfg.SilenceDurationMs,
		SilenceRecoveryMs: cfg.SilenceRecoveryMs,
		PeakHoldMs:        cfg.PeakHoldMs,
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       cfg.SilenceDumpEnabled,
			RetentionDays: cfg.SilenceDumpRetentionDays,
		},

		// Channel imbalance detection
		ChannelImbalanceThreshold:  cfg.ChannelImbalanceThreshold,
		ChannelImbalanceDurationMs: cfg.ChannelImbalanceDurationMs,
		ChannelImbalanceRecoveryMs: cfg.ChannelImbalanceRecoveryMs,

		// Notifications - Webhook
		WebhookHasURL: cfg.WebhookURL != "",
		WebhookEvents: cfg.WebhookEvents,

		// Notifications - Zabbix
		ZabbixServer:       cfg.ZabbixServer,
		ZabbixPort:         cfg.ZabbixPort,
		ZabbixHost:         cfg.ZabbixHost,
		ZabbixSilenceKey:   cfg.ZabbixSilenceKey,
		ZabbixImbalanceKey: cfg.ZabbixImbalanceKey,
		ZabbixUploadKey:    cfg.ZabbixUploadKey,
		ZabbixEvents:       cfg.ZabbixEvents.ToZabbixEventSubscriptions(),

		// Notifications - Email
		GraphTenantID:    cfg.GraphTenantID,
		GraphClientID:    cfg.GraphClientID,
		GraphFromAddress: cfg.GraphFromAddress,
		GraphRecipients:  cfg.GraphRecipients,
		GraphHasSecret:   cfg.GraphClientSecret != "",
		EmailEvents:      cfg.EmailEvents,

		// Recording
		RecordingHasAPIKey:          cfg.RecordingAPIKey != "",
		RecordingMaxDurationMinutes: cfg.RecordingMaxDurationMinutes,
		SRTAvailable:                s.encoder.SRTAvailable(),
		SRTError:                    s.encoder.SRTErrorMessage(),

		// Entities
		Streams:   redactSlice(cfg.Streams, redactStream),
		Recorders: redactSlice(cfg.Recorders, redactRecorder),
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIDevices returns available audio devices.
func (s *Server) handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"devices": audio.Devices(),
	})
}

// handleAPISettings updates all settings atomically.
func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[config.SettingsUpdate](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	audioInputChanged := req.AudioInput != cfg.AudioInput

	preserveHiddenSettings(&req, &cfg)

	if errs := req.Validate(); len(errs) > 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": errs,
		})
		return
	}

	// Apply ALL settings atomically (single lock, single file write)
	if err := s.config.ApplySettings(&req); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Side effects after successful save
	s.encoder.UpdateSilenceConfig()
	s.encoder.UpdateChannelImbalanceConfig()
	s.encoder.UpdateSilenceDumpConfig()
	s.encoder.UpdateRecordingMaxDuration()
	s.encoder.InvalidateGraphSecretExpiryCache()

	// Restart encoder if audio input changed
	if audioInputChanged && s.ffmpegAvailable && s.encoder.State() == types.StateRunning {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() {
				done <- s.encoder.Restart()
			}()

			select {
			case err := <-done:
				if err != nil {
					slog.Error("failed to restart encoder after audio input change", "error", err)
				}
			case <-ctx.Done():
				slog.Error("encoder restart timed out after audio input change")
			}
		}()
	}

	s.broadcastConfigChanged()
	s.writeNoContent(w)
}

// handleListStreams returns all configured streams.
func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	s.writeJSON(w, http.StatusOK, redactSlice(cfg.Streams, redactStream))
}

// handleGetStream returns a single stream by ID.
func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stream := s.config.Stream(id)
	if stream == nil {
		s.writeError(w, http.StatusNotFound, "Stream not found")
		return
	}
	s.writeJSON(w, http.StatusOK, redactStream(stream))
}

// StreamRequest contains fields for creating or updating streams.
type StreamRequest struct {
	// Enabled reports whether the stream is active.
	Enabled bool `json:"enabled"`
	// Mode selects whether the stream pushes to a remote listener or exposes a local listener.
	Mode types.StreamMode `json:"mode"`
	// Host is the SRT server hostname.
	Host string `json:"host"`
	// Port is the SRT server port.
	Port int `json:"port"`
	// Password is the SRT encryption passphrase.
	Password string `json:"password"` //nolint:gosec // G117: intentional field for SRT stream auth
	// ClearPassword removes the saved password when Password is empty.
	ClearPassword bool `json:"clear_password"`
	// StreamID identifies the stream at the destination server.
	StreamID string `json:"stream_id"`
	// Codec selects the audio codec.
	Codec types.Codec `json:"codec"`
	// Bitrate is the encoding bitrate in kbit/s (0 = codec default).
	Bitrate int `json:"bitrate"`
	// MaxRetries is the maximum number of retries before giving up.
	MaxRetries int `json:"max_retries"`
}

func streamRequestDefaults(req *StreamRequest) (types.StreamMode, types.Codec, string) {
	mode := req.Mode.OrDefault()
	codec := req.Codec
	if mode == types.StreamModeListener && codec == "" {
		codec = types.CodecMP3
	}
	host := strings.TrimSpace(req.Host)
	if mode == types.StreamModeListener && host == "" {
		host = types.DefaultListenerBindHost
	}
	return mode, codec, host
}

// handleCreateStream creates a new stream.
func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[StreamRequest](s, w, r)
	if !ok {
		return
	}
	password, err := preserveSecret(req.Password, "", req.ClearPassword, "clear_password", "password")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, codec, host := streamRequestDefaults(&req)

	stream := &types.Stream{
		Enabled:    true,
		Mode:       mode,
		Host:       host,
		Port:       req.Port,
		Password:   password,
		StreamID:   req.StreamID,
		Codec:      codec, // Already validated by UnmarshalJSON when present
		Bitrate:    req.Bitrate,
		MaxRetries: req.MaxRetries,
	}

	// Validate first - client error
	if err := stream.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persistence failures are server errors
	if err := s.config.AddStream(stream); err != nil {
		s.writeConfigError(w, err)
		return
	}

	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StartStream(stream.ID); err != nil {
			slog.Warn("failed to start new stream", "stream_id", stream.ID, "error", err)
		}
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, redactStream(stream))
}

// handleUpdateStream replaces a stream by ID.
func (s *Server) handleUpdateStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing := s.config.Stream(id)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "Stream not found")
		return
	}

	req, ok := parseJSON[StreamRequest](s, w, r)
	if !ok {
		return
	}
	password, err := preserveSecret(req.Password, existing.Password, req.ClearPassword, "clear_password", "password")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, codec, host := streamRequestDefaults(&req)

	// Full replacement - preserve only ID and CreatedAt.
	// For password: empty string means keep existing unless clear_password is true.
	updated := &types.Stream{
		ID:         id,
		Enabled:    req.Enabled,
		Mode:       mode,
		Host:       host,
		Port:       req.Port,
		Password:   password,
		StreamID:   req.StreamID,
		Codec:      codec,
		Bitrate:    req.Bitrate,
		MaxRetries: req.MaxRetries,
		CreatedAt:  existing.CreatedAt,
	}

	// Validate first - client error
	if err := updated.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persistence failures are server errors (not-found can happen on concurrent delete)
	if err := s.config.UpdateStream(updated); err != nil {
		s.writeConfigError(w, err)
		return
	}

	// Restart stream if encoder is running
	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StopStream(id); err != nil {
			//nolint:gosec // G706: structured logging of request-derived ID
			slog.Warn("failed to stop stream for restart", "stream_id", id, "error", err)
		}
		go func() {
			time.Sleep(types.StreamRestartDelay)
			if s.encoder.State() == types.StateRunning {
				if err := s.encoder.StartStream(id); err != nil {
					//nolint:gosec // G706: structured logging of request-derived ID
					slog.Warn("failed to restart stream", "stream_id", id, "error", err)
				}
			}
		}()
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, redactStream(updated))
}

// handleDeleteStream deletes a stream by ID.
func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.config.Stream(id) == nil {
		s.writeError(w, http.StatusNotFound, "Stream not found")
		return
	}

	if err := s.encoder.StopStream(id); err != nil {
		//nolint:gosec // G706: structured logging of request-derived ID
		slog.Warn("failed to stop stream before delete", "stream_id", id, "error", err)
	}

	if err := s.config.RemoveStream(id); err != nil {
		s.writeConfigError(w, err)
		return
	}

	s.broadcastConfigChanged()
	s.writeNoContent(w)
}

// handleListRecorders returns all configured recorders.
func (s *Server) handleListRecorders(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	s.writeJSON(w, http.StatusOK, redactSlice(cfg.Recorders, redactRecorder))
}

// handleGetRecorder returns a single recorder by ID.
func (s *Server) handleGetRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recorder := s.config.Recorder(id)
	if recorder == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}
	s.writeJSON(w, http.StatusOK, redactRecorder(recorder))
}

// RecorderRequest contains fields for creating or updating recorders.
type RecorderRequest struct {
	// Name is the recorder display name.
	Name string `json:"name"`
	// Enabled reports whether the recorder is active.
	Enabled bool `json:"enabled"`
	// Codec selects the recording codec.
	Codec types.Codec `json:"codec"`
	// Bitrate is the encoding bitrate in kbit/s (0 = codec default).
	Bitrate int `json:"bitrate"`
	// RecordingMode selects the recording mode.
	RecordingMode types.RecordingMode `json:"recording_mode"`
	// StorageMode selects local/S3 storage behavior.
	StorageMode types.StorageMode `json:"storage_mode"`
	// LocalPath is the local directory for recordings.
	LocalPath string `json:"local_path"`
	// S3Endpoint is the S3-compatible endpoint URL.
	S3Endpoint string `json:"s3_endpoint"`
	// S3Bucket is the target S3 bucket name.
	S3Bucket string `json:"s3_bucket"`
	// S3AccessKeyID is the S3 access key ID.
	S3AccessKeyID string `json:"s3_access_key_id"`
	// S3SecretAccessKey is the S3 secret for authentication.
	S3SecretAccessKey string `json:"s3_secret_access_key"`
	// ClearS3Secret removes the saved S3 secret when S3SecretAccessKey is empty.
	ClearS3Secret bool `json:"clear_s3_secret"`
	// RetentionDays is the number of days to retain recordings.
	RetentionDays int `json:"retention_days"`
}

// handleCreateRecorder creates a new recorder.
func (s *Server) handleCreateRecorder(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[RecorderRequest](s, w, r)
	if !ok {
		return
	}
	s3Secret, err := preserveSecret(
		req.S3SecretAccessKey,
		"",
		req.ClearS3Secret,
		"clear_s3_secret",
		"s3_secret_access_key",
	)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	recorder := &types.Recorder{
		Name:              req.Name,
		Enabled:           true,
		Codec:             req.Codec, // Already validated by UnmarshalJSON
		Bitrate:           req.Bitrate,
		RecordingMode:     req.RecordingMode, // Already validated by UnmarshalJSON
		StorageMode:       req.StorageMode,   // Already validated by UnmarshalJSON
		LocalPath:         req.LocalPath,
		S3Endpoint:        req.S3Endpoint,
		S3Bucket:          req.S3Bucket,
		S3AccessKeyID:     req.S3AccessKeyID,
		S3SecretAccessKey: s3Secret,
		RetentionDays:     req.RetentionDays,
	}

	// Validate first - client error
	if err := recorder.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRecorderLocalPath(recorder); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persistence/manager failures are server errors
	if err := s.encoder.AddRecorder(recorder); err != nil {
		s.writeConfigError(w, err)
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, redactRecorder(recorder))
}

// handleUpdateRecorder replaces a recorder by ID.
func (s *Server) handleUpdateRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing := s.config.Recorder(id)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	req, ok := parseJSON[RecorderRequest](s, w, r)
	if !ok {
		return
	}
	s3Secret, err := preserveSecret(
		req.S3SecretAccessKey,
		existing.S3SecretAccessKey,
		req.ClearS3Secret,
		"clear_s3_secret",
		"s3_secret_access_key",
	)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Full replacement - preserve only ID and CreatedAt.
	// For S3 secret: empty string means keep existing unless clear_s3_secret is true.
	updated := &types.Recorder{
		ID:                id,
		Name:              req.Name,
		Enabled:           req.Enabled,
		Codec:             req.Codec,
		Bitrate:           req.Bitrate,
		RecordingMode:     req.RecordingMode,
		StorageMode:       req.StorageMode,
		LocalPath:         req.LocalPath,
		S3Endpoint:        req.S3Endpoint,
		S3Bucket:          req.S3Bucket,
		S3AccessKeyID:     req.S3AccessKeyID,
		S3SecretAccessKey: s3Secret,
		RetentionDays:     req.RetentionDays,
		CreatedAt:         existing.CreatedAt,
	}

	// Validate first - client error
	if err := updated.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRecorderLocalPath(updated); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persistence/manager failures are server errors (not-found can happen on concurrent delete)
	if err := s.encoder.UpdateRecorder(updated); err != nil {
		s.writeConfigError(w, err)
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, redactRecorder(updated))
}

// handleDeleteRecorder deletes a recorder by ID.
func (s *Server) handleDeleteRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.config.Recorder(id) == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	if err := s.encoder.RemoveRecorder(id); err != nil {
		s.writeConfigError(w, err)
		return
	}

	s.broadcastConfigChanged()
	s.writeNoContent(w)
}

// handleRecorderAction handles start/stop actions for a recorder.
func (s *Server) handleRecorderAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")

	switch action {
	case "start":
		if s.encoder.State() != types.StateRunning {
			s.writeError(w, http.StatusBadRequest, "Encoder must be running to start recorder")
			return
		}
		if err := s.encoder.StartRecorder(id); err != nil {
			if errors.Is(err, encoder.ErrRecordingNotAvailable) {
				s.writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeMessage(w, "Recorder started")
	case "stop":
		if err := s.encoder.StopRecorder(id); err != nil {
			if errors.Is(err, encoder.ErrRecordingNotAvailable) {
				s.writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeMessage(w, "Recorder stopped")
	default:
		s.writeError(w, http.StatusBadRequest, "invalid action: must be start or stop")
	}
}

// S3TestRequest contains fields for testing S3 connectivity.
type S3TestRequest struct {
	// RecorderID optionally identifies an existing recorder for hidden-secret fallback.
	RecorderID string `json:"recorder_id,omitempty"`
	// Endpoint is the S3-compatible endpoint URL.
	Endpoint string `json:"s3_endpoint"`
	// Bucket is the target S3 bucket name.
	Bucket string `json:"s3_bucket"`
	// AccessKey is the S3 access key ID.
	AccessKey string `json:"s3_access_key_id"` //nolint:gosec // G117: intentional field for S3 auth credentials
	// SecretKey is the S3 secret for authentication.
	SecretKey string `json:"s3_secret_access_key"`
}

func (s *Server) resolveS3TestSecret(req *S3TestRequest) (string, bool) {
	if req.SecretKey != "" || req.RecorderID == "" {
		return req.SecretKey, true
	}

	recorder := s.config.Recorder(req.RecorderID)
	if recorder == nil {
		return "", false
	}
	return recorder.S3SecretAccessKey, true
}

// handleTestS3 tests S3 connectivity.
func (s *Server) handleTestS3(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[S3TestRequest](s, w, r)
	if !ok {
		return
	}

	secretKey, ok := s.resolveS3TestSecret(&req)
	if !ok {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	for _, issue := range types.ValidateS3Credentials(req.Bucket, req.AccessKey, secretKey) {
		s.writeError(w, http.StatusBadRequest, issue.Field+" is required")
		return
	}

	cfg := &types.Recorder{
		S3Endpoint:        req.Endpoint,
		S3Bucket:          req.Bucket,
		S3AccessKeyID:     req.AccessKey,
		S3SecretAccessKey: secretKey,
	}

	if err := recording.TestRecorderS3Connection(cfg); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.writeMessage(w, "S3 connection successful")
}

// NotificationTestRequest contains fields for testing notifications.
//
// Pointer fields distinguish omitted values from explicit overrides when
// notification test handlers merge request fields with saved config.
type NotificationTestRequest struct {
	WebhookURL *string `json:"webhook_url,omitempty"`

	GraphTenantID     *string `json:"graph_tenant_id,omitempty"`
	GraphClientID     *string `json:"graph_client_id,omitempty"`
	GraphClientSecret *string `json:"graph_client_secret,omitempty"`
	GraphFromAddress  *string `json:"graph_from_address,omitempty"`
	GraphRecipients   *string `json:"graph_recipients,omitempty"`

	ZabbixServer       *string `json:"zabbix_server,omitempty"`
	ZabbixPort         *int    `json:"zabbix_port,omitempty"`
	ZabbixHost         *string `json:"zabbix_host,omitempty"`
	ZabbixSilenceKey   *string `json:"zabbix_silence_key,omitempty"`
	ZabbixImbalanceKey *string `json:"zabbix_imbalance_key,omitempty"`
	ZabbixUploadKey    *string `json:"zabbix_upload_key,omitempty"`
}

// deref returns *p when p is non-nil, otherwise fallback. Used by notification-test
// handlers to merge request fields with saved config: omitted JSON fields fall back,
// explicit values (including "") are used as-is.
func deref[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
}

// preserveSecret resolves a submitted secret against the stored one: empty
// keeps the saved value, non-empty replaces it, and the clear flag blanks it.
// A clear flag combined with a new value is rejected as ambiguous.
func preserveSecret(value, saved string, clearRequested bool, clearField, valueField string) (string, error) {
	if clearRequested && value != "" {
		return "", fmt.Errorf("%s: conflicts with non-empty %s", clearField, valueField)
	}
	if clearRequested {
		return "", nil
	}
	return cmp.Or(value, saved), nil
}

// preserveHiddenSettings keeps saved hidden values when settings forms submit
// empty placeholders. Clear flags bypass preservation so Validate can report
// conflicts with newly submitted values.
func preserveHiddenSettings(req *config.SettingsUpdate, cfg *config.Snapshot) {
	if !req.ClearWebhookURL {
		req.WebhookURL = cmp.Or(req.WebhookURL, cfg.WebhookURL)
	}
	if !req.ClearGraphClientSecret {
		req.GraphClientSecret = cmp.Or(req.GraphClientSecret, cfg.GraphClientSecret)
	}
}

// handleAPITestWebhook tests webhook notification connectivity.
func (s *Server) handleAPITestWebhook(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	webhookURL := deref(req.WebhookURL, cfg.WebhookURL)

	if issues := types.ValidateWebhookURL(webhookURL, validation.RequireComplete); len(issues) > 0 {
		s.writeError(w, http.StatusBadRequest, "No webhook URL configured")
		return
	}

	if err := notify.SendWebhookTest(webhookURL, cfg.StationName); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.writeMessage(w, "Webhook test sent")
}

// handleAPITestEmail tests email notification.
func (s *Server) handleAPITestEmail(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	tenantID := deref(req.GraphTenantID, cfg.GraphTenantID)
	clientID := deref(req.GraphClientID, cfg.GraphClientID)
	clientSecret := deref(req.GraphClientSecret, cfg.GraphClientSecret)
	fromAddress := deref(req.GraphFromAddress, cfg.GraphFromAddress)
	recipients := deref(req.GraphRecipients, cfg.GraphRecipients)

	graphCfg := &notify.GraphConfig{
		TenantID:     tenantID,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		FromAddress:  fromAddress,
		Recipients:   recipients,
	}

	if issues := graphCfg.CredentialsIssues(); len(issues) > 0 {
		s.writeError(w, http.StatusBadRequest, "Email not fully configured")
		return
	}

	if err := notify.SendTestEmail(graphCfg, cfg.StationName); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.writeMessage(w, "Test email sent")
}

// handleAPITestZabbix tests Zabbix trapper notification connectivity.
func (s *Server) handleAPITestZabbix(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	server := deref(req.ZabbixServer, cfg.ZabbixServer)
	port := deref(req.ZabbixPort, cfg.ZabbixPort)
	host := deref(req.ZabbixHost, cfg.ZabbixHost)
	silenceKey := deref(req.ZabbixSilenceKey, cfg.ZabbixSilenceKey)
	imbalanceKey := deref(req.ZabbixImbalanceKey, cfg.ZabbixImbalanceKey)
	uploadKey := deref(req.ZabbixUploadKey, cfg.ZabbixUploadKey)

	if issues := types.ValidateZabbixConfigured(server, host, silenceKey, imbalanceKey, uploadKey); len(issues) > 0 {
		s.writeError(w, http.StatusBadRequest, "Zabbix not fully configured")
		return
	}

	if silenceKey != "" {
		if err := notify.SendZabbixTest(server, port, host, silenceKey); err != nil {
			s.writeError(w, http.StatusBadGateway, "silence key: "+err.Error())
			return
		}
	}
	if imbalanceKey != "" {
		if err := notify.SendZabbixTest(server, port, host, imbalanceKey); err != nil {
			s.writeError(w, http.StatusBadGateway, "imbalance key: "+err.Error())
			return
		}
	}
	if uploadKey != "" {
		if err := notify.SendZabbixTest(server, port, host, uploadKey); err != nil {
			s.writeError(w, http.StatusBadGateway, "upload key: "+err.Error())
			return
		}
	}

	s.writeMessage(w, "Zabbix test sent")
}

// handleAPIRegenerateKey generates a new recording API key.
func (s *Server) handleAPIRegenerateKey(w http.ResponseWriter, r *http.Request) {
	newKey, err := config.GenerateAPIKey()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.config.SetRecordingAPIKey(newKey); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, map[string]string{"api_key": newKey})
}

// HealthResponse is the response body for the health endpoint.
type HealthResponse struct {
	// Status is the overall health status (healthy or unhealthy).
	Status string `json:"status"`
	// EncoderState is the encoder's current state string.
	EncoderState string `json:"encoder_state"`
	// StreamCount is the number of configured streams.
	StreamCount int `json:"stream_count"`
	// StreamsStable is the number of stable streams.
	StreamsStable int `json:"streams_stable"`
	// RecorderCount is the number of configured recorders.
	RecorderCount int `json:"recorder_count"`
	// RecordersRunning is the number of running recorders.
	RecordersRunning int `json:"recorders_running"`
	// UptimeSeconds is the encoder uptime in seconds.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// SilenceDetected reports whether silence is currently detected.
	SilenceDetected bool `json:"silence_detected"`
	// ChannelImbalanceDetected reports whether channel imbalance is active.
	// It is informational and does not affect health status.
	ChannelImbalanceDetected bool `json:"channel_imbalance_detected"`
}

// ReadyResponse is the response body for the readiness endpoint.
type ReadyResponse struct {
	Status     string                    `json:"status"`
	Components map[string]ReadyComponent `json:"components"`
}

// ReadyComponent reports readiness for one subsystem.
type ReadyComponent struct {
	OK      bool           `json:"ok"`
	Message string         `json:"message,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

const readinessMaxPendingUploads = 0

// handleHealth returns the health status of the encoder.
// It returns 200 OK if healthy, 503 Service Unavailable if unhealthy.
// Health is defined as: encoder running AND FFmpeg available.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	resp, httpStatus := buildHealthResponse(&healthInputs{
		ffmpegAvailable:  s.ffmpegAvailable,
		encoderStatus:    s.encoder.Status(),
		streams:          cfg.Streams,
		streamStatuses:   s.encoder.StreamStatuses(cfg.Streams),
		recorders:        cfg.Recorders,
		recorderStatuses: s.encoder.RecorderStatuses(),
		audioLevels:      s.encoder.AudioLevels(),
	})
	s.writeJSON(w, httpStatus, resp)
}

type healthInputs struct {
	ffmpegAvailable  bool
	encoderStatus    types.EncoderStatus
	streams          []types.Stream
	streamStatuses   map[string]types.ProcessStatus
	recorders        []types.Recorder
	recorderStatuses map[string]types.ProcessStatus
	audioLevels      audio.AudioLevels
}

// buildHealthResponse assembles /health without treating audio conditions as failures.
// Use /ready for broadcast-impacting conditions such as silence or imbalance.
func buildHealthResponse(in *healthInputs) (resp HealthResponse, httpStatus int) {
	isHealthy := in.ffmpegAvailable && in.encoderStatus.State == types.StateRunning

	status := "healthy"
	httpStatus = http.StatusOK
	if !isHealthy {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	}

	return HealthResponse{
		Status:                   status,
		EncoderState:             string(in.encoderStatus.State),
		StreamCount:              len(in.streams),
		StreamsStable:            countStableStreams(in.streamStatuses),
		RecorderCount:            len(in.recorders),
		RecordersRunning:         countRunningRecorders(in.recorderStatuses),
		UptimeSeconds:            in.encoderStatus.UptimeSeconds,
		SilenceDetected:          in.audioLevels.SilenceLevel == audio.SilenceLevelActive,
		ChannelImbalanceDetected: in.audioLevels.ChannelImbalanceLevel == audio.ImbalanceLevelActive,
	}, httpStatus
}

// handleReady returns broadcast-readiness status for production monitoring.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	encoderStatus := s.encoder.Status()
	resp, httpStatus := buildReadyResponse(&readyInputs{
		ffmpegAvailable:    s.ffmpegAvailable,
		recordingAvailable: s.encoder.RecordingAvailable(),
		encoderStatus:      encoderStatus,
		streams:            cfg.Streams,
		streamStatuses:     s.encoder.StreamStatuses(cfg.Streams),
		recorders:          cfg.Recorders,
		recorderStatuses:   s.encoder.RecorderStatuses(),
		audioLevels:        s.encoder.AudioLevels(),
		pendingUploads:     s.encoder.PendingUploadCount(),
	})
	s.writeJSON(w, httpStatus, resp)
}

type readyInputs struct {
	ffmpegAvailable    bool
	recordingAvailable bool
	encoderStatus      types.EncoderStatus
	streams            []types.Stream
	streamStatuses     map[string]types.ProcessStatus
	recorders          []types.Recorder
	recorderStatuses   map[string]types.ProcessStatus
	audioLevels        audio.AudioLevels
	pendingUploads     int
}

func buildReadyResponse(in *readyInputs) (resp ReadyResponse, httpStatus int) {
	components := map[string]ReadyComponent{
		"process":           readyProcess(in.ffmpegAvailable, &in.encoderStatus),
		"streams":           readyStreams(in.streams, in.streamStatuses),
		"silence":           readySilence(&in.audioLevels),
		"channel_imbalance": readyChannelImbalance(&in.audioLevels),
		"recorders":         readyRecorders(in.recordingAvailable, in.recorders, in.recorderStatuses),
		"uploads":           readyUploads(in.pendingUploads),
	}

	status := "ready"
	httpStatus = http.StatusOK
	for _, component := range components {
		if !component.OK {
			status = "not_ready"
			httpStatus = http.StatusServiceUnavailable
			break
		}
	}

	return ReadyResponse{
		Status:     status,
		Components: components,
	}, httpStatus
}

func readyProcess(ffmpegAvailable bool, status *types.EncoderStatus) ReadyComponent {
	details := map[string]any{
		"ffmpeg_available": ffmpegAvailable,
		"encoder_state":    status.State,
	}
	if !ffmpegAvailable {
		return ReadyComponent{OK: false, Message: "ffmpeg is not available", Details: details}
	}
	if status.State != types.StateRunning {
		return ReadyComponent{OK: false, Message: "encoder is not running", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func readyStreams(streams []types.Stream, statuses map[string]types.ProcessStatus) ReadyComponent {
	enabled := 0
	monitored := 0
	notReady := []string{}
	for i := range streams {
		stream := &streams[i]
		if !stream.Enabled {
			continue
		}
		enabled++
		if stream.ModeOrDefault() == types.StreamModeListener {
			continue
		}
		monitored++
		status := statuses[stream.ID]
		if status.State == types.ProcessRunning && status.Stable && !status.Exhausted {
			continue
		}
		notReady = append(notReady, stream.ID)
	}

	details := map[string]any{
		"enabled":              enabled,
		"production_monitored": monitored,
		"not_ready":            notReady,
	}
	if len(notReady) > 0 {
		return ReadyComponent{OK: false, Message: "one or more enabled streams are not stable", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func readySilence(levels *audio.AudioLevels) ReadyComponent {
	details := map[string]any{
		"silence_level": levels.SilenceLevel,
		"left_db":       levels.Left,
		"right_db":      levels.Right,
	}
	if levels.SilenceLevel == audio.SilenceLevelActive {
		return ReadyComponent{OK: false, Message: "silence is active", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func readyChannelImbalance(levels *audio.AudioLevels) ReadyComponent {
	details := map[string]any{
		"channel_imbalance_level": levels.ChannelImbalanceLevel,
		"balance_db":              levels.BalanceDB,
		"imbalance_db":            levels.ImbalanceDB,
	}
	if levels.ChannelImbalanceLevel == audio.ImbalanceLevelActive {
		return ReadyComponent{OK: false, Message: "channel imbalance is active", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func readyRecorders(
	recordingAvailable bool,
	recorders []types.Recorder,
	statuses map[string]types.ProcessStatus,
) ReadyComponent {
	enabled := 0
	notReady := []string{}
	for i := range recorders {
		recorder := &recorders[i]
		if !recorder.Enabled {
			continue
		}
		enabled++
		status := statuses[recorder.ID]
		if status.State == types.ProcessError {
			notReady = append(notReady, recorder.ID)
			continue
		}
		if recorder.RecordingMode == types.RecordingHourly &&
			status.State != types.ProcessRunning &&
			status.State != types.ProcessRotating {
			notReady = append(notReady, recorder.ID)
		}
	}

	details := map[string]any{
		"enabled":             enabled,
		"recording_available": recordingAvailable,
		"not_ready":           notReady,
	}
	if enabled > 0 && !recordingAvailable {
		return ReadyComponent{OK: false, Message: "recording manager is not available", Details: details}
	}
	if len(notReady) > 0 {
		return ReadyComponent{OK: false, Message: "one or more enabled recorders are not ready", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func readyUploads(pending int) ReadyComponent {
	details := map[string]any{
		"pending": pending,
		"max":     readinessMaxPendingUploads,
	}
	if pending > readinessMaxPendingUploads {
		return ReadyComponent{OK: false, Message: "recording uploads are pending retry", Details: details}
	}
	return ReadyComponent{OK: true, Details: details}
}

func countStableStreams(statuses map[string]types.ProcessStatus) int {
	count := 0
	for _, status := range statuses {
		if status.Stable {
			count++
		}
	}
	return count
}

func countRunningRecorders(statuses map[string]types.ProcessStatus) int {
	count := 0
	for _, status := range statuses {
		if status.State == types.ProcessRunning {
			count++
		}
	}
	return count
}

// handleAPIEvents returns decorated event history from the active log.
func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	logPath := ""
	if s.encoder != nil {
		logPath = s.encoder.EventLogPath()
	}
	s.handleAPIEventsFromPath(w, r, logPath)
}

func (s *Server) handleAPIEventsFromPath(w http.ResponseWriter, r *http.Request, logPath string) {
	emptyResponse := map[string]any{
		"events":   []eventlog.EventView{},
		"groups":   eventlog.EmptyEventGroups(),
		"has_more": false,
	}

	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = min(parsed, eventlog.MaxReadLimit)
		}
	}

	offset := 0
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if parsed, err := strconv.Atoi(offsetStr); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	var filter eventlog.TypeFilter
	switch r.URL.Query().Get("type") {
	case "stream":
		filter = eventlog.FilterStream
	case "audio":
		filter = eventlog.FilterAudio
	case "recorder":
		filter = eventlog.FilterRecorder
	default:
		filter = eventlog.FilterAll
	}

	if logPath == "" {
		s.writeJSON(w, http.StatusOK, emptyResponse)
		return
	}

	eventList, hasMore, err := eventlog.ReadLast(logPath, limit, offset, filter)
	if err != nil {
		slog.Warn("failed to read events", "error", err)
		s.writeError(w, http.StatusInternalServerError, "Could not read event history")
		return
	}

	views := eventlog.DecorateEvents(eventList)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"events":   views,
		"groups":   eventlog.GroupEvents(views),
		"has_more": hasMore,
	})
}
