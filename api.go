package main

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
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

// handleAPIConfig returns the full configuration for the frontend.
func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()

	resp := types.APIConfigResponse{
		// Audio
		AudioInput: cfg.AudioInput,
		Devices:    audio.ListDevices(),
		Platform:   runtime.GOOS,

		// Silence detection
		SilenceThreshold:  cfg.SilenceThreshold,
		SilenceDurationMs: cfg.SilenceDurationMs,
		SilenceRecoveryMs: cfg.SilenceRecoveryMs,
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       cfg.SilenceDumpEnabled,
			RetentionDays: cfg.SilenceDumpRetentionDays,
		},

		// Notifications - Webhook
		WebhookURL: cfg.WebhookURL,

		// Notifications - Zabbix
		ZabbixServer: cfg.ZabbixServer,
		ZabbixPort:   cfg.ZabbixPort,
		ZabbixHost:   cfg.ZabbixHost,
		ZabbixKey:    cfg.ZabbixKey,

		// Notifications - Email
		GraphTenantID:    cfg.GraphTenantID,
		GraphClientID:    cfg.GraphClientID,
		GraphFromAddress: cfg.GraphFromAddress,
		GraphRecipients:  cfg.GraphRecipients,
		GraphHasSecret:   cfg.GraphClientSecret != "",

		// Recording
		RecordingAPIKey: cfg.RecordingAPIKey,

		// Entities
		Streams:   cfg.Streams,
		Recorders: cfg.Recorders,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIDevices returns available audio devices.
func (s *Server) handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"devices": audio.ListDevices(),
	})
}

// handleAPISettings updates all settings atomically.
func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[config.SettingsUpdate](s, w, r)
	if !ok {
		return
	}

	// Validate ALL settings upfront (no side effects)
	if errs := req.Validate(); len(errs) > 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": errs,
		})
		return
	}

	cfg := s.config.Snapshot()
	audioInputChanged := req.AudioInput != cfg.AudioInput

	// Preserve existing secret if not provided (empty = keep existing)
	req.GraphClientSecret = cmp.Or(req.GraphClientSecret, cfg.GraphClientSecret)

	// Apply ALL settings atomically (single lock, single file write)
	if err := s.config.ApplySettings(&req); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Side effects after successful save
	s.encoder.UpdateSilenceConfig()
	s.encoder.UpdateSilenceDumpConfig()
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
	s.writeJSON(w, http.StatusOK, cfg.Streams)
}

// handleGetStream returns a single stream by ID.
func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stream := s.config.Stream(id)
	if stream == nil {
		s.writeError(w, http.StatusNotFound, "Stream not found")
		return
	}
	s.writeJSON(w, http.StatusOK, stream)
}

// StreamRequest contains fields for creating or updating streams.
type StreamRequest struct {
	Enabled    bool        `json:"enabled"`
	Host       string      `json:"host"`
	Port       int         `json:"port"`
	Password   string      `json:"password"`
	StreamID   string      `json:"stream_id"`
	Codec      types.Codec `json:"codec"`
	MaxRetries int         `json:"max_retries"`
}

// handleCreateStream creates a new stream.
func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[StreamRequest](s, w, r)
	if !ok {
		return
	}

	stream := &types.Stream{
		Enabled:    true,
		Host:       req.Host,
		Port:       req.Port,
		Password:   req.Password,
		StreamID:   req.StreamID,
		Codec:      req.Codec, // Already validated by UnmarshalJSON
		MaxRetries: req.MaxRetries,
	}

	if err := s.config.AddStream(stream); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StartStream(stream.ID); err != nil {
			slog.Warn("failed to start new stream", "stream_id", stream.ID, "error", err)
		}
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, stream)
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

	// Full replacement - preserve only ID and CreatedAt
	// For password: empty string means "keep existing" (not sent from frontend for security)
	updated := &types.Stream{
		ID:         id,
		Enabled:    req.Enabled,
		Host:       req.Host,
		Port:       req.Port,
		Password:   cmp.Or(req.Password, existing.Password),
		StreamID:   req.StreamID,
		Codec:      req.Codec,
		MaxRetries: req.MaxRetries,
		CreatedAt:  existing.CreatedAt,
	}

	if err := s.config.UpdateStream(updated); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Restart stream if encoder is running
	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StopStream(id); err != nil {
			slog.Warn("failed to stop stream for restart", "stream_id", id, "error", err)
		}
		go func() {
			time.Sleep(types.StreamRestartDelay)
			if s.encoder.State() == types.StateRunning {
				if err := s.encoder.StartStream(id); err != nil {
					slog.Warn("failed to restart stream", "stream_id", id, "error", err)
				}
			}
		}()
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, updated)
}

// handleDeleteStream deletes a stream by ID.
func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.config.Stream(id) == nil {
		s.writeError(w, http.StatusNotFound, "Stream not found")
		return
	}

	if err := s.encoder.StopStream(id); err != nil {
		slog.Warn("failed to stop stream before delete", "stream_id", id, "error", err)
	}

	if err := s.config.RemoveStream(id); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeNoContent(w)
}

// handleListRecorders returns all configured recorders.
func (s *Server) handleListRecorders(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	s.writeJSON(w, http.StatusOK, cfg.Recorders)
}

// handleGetRecorder returns a single recorder by ID.
func (s *Server) handleGetRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recorder := s.config.Recorder(id)
	if recorder == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}
	s.writeJSON(w, http.StatusOK, recorder)
}

// RecorderRequest contains fields for creating or updating recorders.
type RecorderRequest struct {
	Name              string             `json:"name"`
	Enabled           bool               `json:"enabled"`
	Codec             types.Codec        `json:"codec"`
	RotationMode      types.RotationMode `json:"rotation_mode"`
	StorageMode       types.StorageMode  `json:"storage_mode"`
	LocalPath         string             `json:"local_path"`
	S3Endpoint        string             `json:"s3_endpoint"`
	S3Bucket          string             `json:"s3_bucket"`
	S3AccessKeyID     string             `json:"s3_access_key_id"`
	S3SecretAccessKey string             `json:"s3_secret_access_key"`
	RetentionDays     int                `json:"retention_days"`
}

// handleCreateRecorder creates a new recorder.
func (s *Server) handleCreateRecorder(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[RecorderRequest](s, w, r)
	if !ok {
		return
	}

	recorder := &types.Recorder{
		Name:              req.Name,
		Enabled:           true,
		Codec:             req.Codec,        // Already validated by UnmarshalJSON
		RotationMode:      req.RotationMode, // Already validated by UnmarshalJSON
		StorageMode:       req.StorageMode,  // Already validated by UnmarshalJSON
		LocalPath:         req.LocalPath,
		S3Endpoint:        req.S3Endpoint,
		S3Bucket:          req.S3Bucket,
		S3AccessKeyID:     req.S3AccessKeyID,
		S3SecretAccessKey: req.S3SecretAccessKey,
		RetentionDays:     req.RetentionDays,
	}

	if err := s.encoder.AddRecorder(recorder); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, recorder)
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

	// Full replacement - preserve only ID and CreatedAt
	// For S3 secret: empty string means "keep existing" (not sent from frontend for security)
	updated := &types.Recorder{
		ID:                id,
		Name:              req.Name,
		Enabled:           req.Enabled,
		Codec:             req.Codec,
		RotationMode:      req.RotationMode,
		StorageMode:       req.StorageMode,
		LocalPath:         req.LocalPath,
		S3Endpoint:        req.S3Endpoint,
		S3Bucket:          req.S3Bucket,
		S3AccessKeyID:     req.S3AccessKeyID,
		S3SecretAccessKey: cmp.Or(req.S3SecretAccessKey, existing.S3SecretAccessKey),
		RetentionDays:     req.RetentionDays,
		CreatedAt:         existing.CreatedAt,
	}

	if err := s.encoder.UpdateRecorder(updated); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, updated)
}

// handleDeleteRecorder deletes a recorder by ID.
func (s *Server) handleDeleteRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.config.Recorder(id) == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	if err := s.encoder.RemoveRecorder(id); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
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
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeMessage(w, "Recorder started")
	case "stop":
		if err := s.encoder.StopRecorder(id); err != nil {
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
	Endpoint  string `json:"s3_endpoint"`
	Bucket    string `json:"s3_bucket"`
	AccessKey string `json:"s3_access_key_id"`
	SecretKey string `json:"s3_secret_access_key"`
}

// handleTestS3 tests S3 connectivity.
func (s *Server) handleTestS3(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[S3TestRequest](s, w, r)
	if !ok {
		return
	}

	if req.Bucket == "" {
		s.writeError(w, http.StatusBadRequest, "s3_bucket is required")
		return
	}
	if req.AccessKey == "" {
		s.writeError(w, http.StatusBadRequest, "s3_access_key_id is required")
		return
	}
	if req.SecretKey == "" {
		s.writeError(w, http.StatusBadRequest, "s3_secret_access_key is required")
		return
	}

	cfg := &types.Recorder{
		S3Endpoint:        req.Endpoint,
		S3Bucket:          req.Bucket,
		S3AccessKeyID:     req.AccessKey,
		S3SecretAccessKey: req.SecretKey,
	}

	if err := recording.TestRecorderS3Connection(cfg); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.writeMessage(w, "S3 connection successful")
}

// NotificationTestRequest contains fields for testing notifications.
type NotificationTestRequest struct {
	// Webhook
	WebhookURL string `json:"webhook_url,omitempty"`

	// Email
	GraphTenantID     string `json:"graph_tenant_id,omitempty"`
	GraphClientID     string `json:"graph_client_id,omitempty"`
	GraphClientSecret string `json:"graph_client_secret,omitempty"`
	GraphFromAddress  string `json:"graph_from_address,omitempty"`
	GraphRecipients   string `json:"graph_recipients,omitempty"`

	// Zabbix
	ZabbixServer string `json:"zabbix_server,omitempty"`
	ZabbixPort   int    `json:"zabbix_port,omitempty"`
	ZabbixHost   string `json:"zabbix_host,omitempty"`
	ZabbixKey    string `json:"zabbix_key,omitempty"`
}

// handleAPITestWebhook tests webhook notification connectivity.
func (s *Server) handleAPITestWebhook(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	webhookURL := cmp.Or(req.WebhookURL, cfg.WebhookURL)

	if webhookURL == "" {
		s.writeError(w, http.StatusBadRequest, "No webhook URL configured")
		return
	}

	if err := notify.SendTestWebhook(webhookURL, cfg.StationName); err != nil {
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
	tenantID := cmp.Or(req.GraphTenantID, cfg.GraphTenantID)
	clientID := cmp.Or(req.GraphClientID, cfg.GraphClientID)
	clientSecret := cmp.Or(req.GraphClientSecret, cfg.GraphClientSecret)
	fromAddress := cmp.Or(req.GraphFromAddress, cfg.GraphFromAddress)
	recipients := cmp.Or(req.GraphRecipients, cfg.GraphRecipients)

	if tenantID == "" || clientID == "" || clientSecret == "" {
		s.writeError(w, http.StatusBadRequest, "Email not fully configured")
		return
	}

	graphCfg := &notify.GraphConfig{
		TenantID:     tenantID,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		FromAddress:  fromAddress,
		Recipients:   recipients,
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
	server := cmp.Or(req.ZabbixServer, cfg.ZabbixServer)
	port := cmp.Or(req.ZabbixPort, cfg.ZabbixPort)
	host := cmp.Or(req.ZabbixHost, cfg.ZabbixHost)
	key := cmp.Or(req.ZabbixKey, cfg.ZabbixKey)

	if server == "" || host == "" || key == "" {
		s.writeError(w, http.StatusBadRequest, "Zabbix not fully configured")
		return
	}

	if err := notify.SendTestZabbix(server, port, host, key); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
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

// handleAPIEvents returns events from the event log.
func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	emptyResponse := map[string]any{
		"events":   []eventlog.Event{},
		"has_more": false,
	}

	// Parse limit parameter (default 50, max MaxReadLimit)
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = min(parsed, eventlog.MaxReadLimit)
		}
	}

	// Parse offset parameter (default 0)
	offset := 0
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if parsed, err := strconv.Atoi(offsetStr); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	// Parse type filter parameter
	var filter eventlog.TypeFilter
	switch r.URL.Query().Get("type") {
	case "stream":
		filter = eventlog.FilterStream
	case "silence":
		filter = eventlog.FilterSilence
	case "recorder":
		filter = eventlog.FilterRecorder
	default:
		filter = eventlog.FilterAll
	}

	// Get event log path from encoder
	logPath := s.encoder.EventLogPath()
	if logPath == "" {
		s.writeJSON(w, http.StatusOK, emptyResponse)
		return
	}

	// Read events from log file
	eventList, hasMore, err := eventlog.ReadLast(logPath, limit, offset, filter)
	if err != nil {
		slog.Warn("failed to read events", "error", err)
		s.writeJSON(w, http.StatusOK, emptyResponse)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"events":   eventList,
		"has_more": hasMore,
	})
}
