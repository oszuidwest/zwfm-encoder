package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// API response helpers

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

func (s *Server) readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// handleAPIConfig returns the full configuration for the frontend.
// GET /api/config
func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

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
		WebhookURL:    cfg.WebhookURL,
		WebhookEvents: s.getWebhookEvents(),

		// Notifications - Log
		LogPath:   cfg.LogPath,
		LogEvents: s.getLogEvents(),

		// Notifications - Zabbix
		ZabbixServer: cfg.ZabbixServer,
		ZabbixPort:   cfg.ZabbixPort,
		ZabbixHost:   cfg.ZabbixHost,
		ZabbixKey:    cfg.ZabbixKey,
		ZabbixEvents: s.getZabbixEvents(),

		// Notifications - Email
		GraphTenantID:    cfg.GraphTenantID,
		GraphClientID:    cfg.GraphClientID,
		GraphFromAddress: cfg.GraphFromAddress,
		GraphRecipients:  cfg.GraphRecipients,
		GraphHasSecret:   cfg.GraphClientSecret != "",
		GraphEvents:      s.getGraphEvents(),

		// Recording
		RecordingAPIKey: cfg.RecordingAPIKey,

		// Entities
		Outputs:   cfg.Outputs,
		Recorders: cfg.Recorders,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIDevices returns available audio devices.
// GET /api/devices
func (s *Server) handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"devices": audio.ListDevices(),
	})
}

// SettingsUpdateRequest is the request body for POST /api/settings.
type SettingsUpdateRequest struct {
	// Audio
	AudioInput *string `json:"audio_input"`

	// Silence detection
	SilenceThreshold  *float64 `json:"silence_threshold"`
	SilenceDurationMs *int64   `json:"silence_duration_ms"`
	SilenceRecoveryMs *int64   `json:"silence_recovery_ms"`
	SilenceDumpEnabled *bool   `json:"silence_dump_enabled"`
	SilenceDumpRetentionDays *int `json:"silence_dump_retention_days"`

	// Webhook
	WebhookURL    *string `json:"webhook_url"`
	WebhookEvents *types.EventConfig `json:"webhook_events"`

	// Log
	LogPath   *string `json:"log_path"`
	LogEvents *types.EventConfig `json:"log_events"`

	// Zabbix
	ZabbixServer *string `json:"zabbix_server"`
	ZabbixPort   *int    `json:"zabbix_port"`
	ZabbixHost   *string `json:"zabbix_host"`
	ZabbixKey    *string `json:"zabbix_key"`
	ZabbixEvents *types.EventConfig `json:"zabbix_events"`

	// Email (Graph)
	GraphTenantID    *string `json:"graph_tenant_id"`
	GraphClientID    *string `json:"graph_client_id"`
	GraphClientSecret *string `json:"graph_client_secret"`
	GraphFromAddress *string `json:"graph_from_address"`
	GraphRecipients  *string `json:"graph_recipients"`
	GraphEvents      *types.EventConfig `json:"graph_events"`
}

// handleAPISettings updates all settings atomically.
// POST /api/settings
func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req SettingsUpdateRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Track if audio input changed (requires encoder restart)
	cfg := s.config.Snapshot()
	audioInputChanged := req.AudioInput != nil && *req.AudioInput != cfg.AudioInput

	// Apply all settings in groups
	if err := s.applyAudioSettings(&req); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.applySilenceSettings(&req, &cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.applyNotificationSettings(&req, &cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update encoder's silence config
	s.encoder.UpdateSilenceConfig()

	// Restart encoder if audio input changed
	if audioInputChanged && s.ffmpegAvailable {
		go func() {
			if s.encoder.State() == types.StateRunning {
				if err := s.encoder.Restart(); err != nil {
					slog.Error("failed to restart encoder after audio input change", "error", err)
				}
			}
		}()
	}

	// Broadcast config change to WebSocket clients
	s.broadcastConfigChanged()

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// applyAudioSettings applies audio-related settings from the request.
func (s *Server) applyAudioSettings(req *SettingsUpdateRequest) error {
	if req.AudioInput != nil {
		if err := s.config.SetAudioInput(*req.AudioInput); err != nil {
			return err
		}
	}
	return nil
}

// applySilenceSettings applies silence detection settings from the request.
func (s *Server) applySilenceSettings(req *SettingsUpdateRequest, cfg *config.Snapshot) error {
	if req.SilenceThreshold != nil {
		if err := s.config.SetSilenceThreshold(*req.SilenceThreshold); err != nil {
			return err
		}
	}

	if req.SilenceDurationMs != nil {
		if err := s.config.SetSilenceDurationMs(*req.SilenceDurationMs); err != nil {
			return err
		}
	}

	if req.SilenceRecoveryMs != nil {
		if err := s.config.SetSilenceRecoveryMs(*req.SilenceRecoveryMs); err != nil {
			return err
		}
	}

	if req.SilenceDumpEnabled != nil || req.SilenceDumpRetentionDays != nil {
		enabled := cfg.SilenceDumpEnabled
		retention := cfg.SilenceDumpRetentionDays
		if req.SilenceDumpEnabled != nil {
			enabled = *req.SilenceDumpEnabled
		}
		if req.SilenceDumpRetentionDays != nil {
			retention = *req.SilenceDumpRetentionDays
		}
		if err := s.config.SetSilenceDump(enabled, retention); err != nil {
			return err
		}
	}

	return nil
}

// applyNotificationSettings applies notification settings from the request.
func (s *Server) applyNotificationSettings(req *SettingsUpdateRequest, cfg *config.Snapshot) error {
	if req.WebhookURL != nil {
		if err := s.config.SetWebhookURL(*req.WebhookURL); err != nil {
			return err
		}
	}

	if req.LogPath != nil {
		if err := s.config.SetLogPath(*req.LogPath); err != nil {
			return err
		}
	}

	if err := s.applyZabbixSettings(req, cfg); err != nil {
		return err
	}

	if err := s.applyGraphSettings(req, cfg); err != nil {
		return err
	}

	return nil
}

// applyZabbixSettings applies Zabbix notification settings.
func (s *Server) applyZabbixSettings(req *SettingsUpdateRequest, cfg *config.Snapshot) error {
	if req.ZabbixServer == nil && req.ZabbixPort == nil && req.ZabbixHost == nil && req.ZabbixKey == nil {
		return nil
	}

	server := cfg.ZabbixServer
	port := cfg.ZabbixPort
	host := cfg.ZabbixHost
	key := cfg.ZabbixKey
	if req.ZabbixServer != nil {
		server = *req.ZabbixServer
	}
	if req.ZabbixPort != nil {
		port = *req.ZabbixPort
	}
	if req.ZabbixHost != nil {
		host = *req.ZabbixHost
	}
	if req.ZabbixKey != nil {
		key = *req.ZabbixKey
	}
	return s.config.SetZabbixConfig(server, port, host, key)
}

// applyGraphSettings applies Microsoft Graph email settings.
func (s *Server) applyGraphSettings(req *SettingsUpdateRequest, cfg *config.Snapshot) error {
	if req.GraphTenantID == nil && req.GraphClientID == nil && req.GraphClientSecret == nil &&
		req.GraphFromAddress == nil && req.GraphRecipients == nil {
		return nil
	}

	tenantID := cfg.GraphTenantID
	clientID := cfg.GraphClientID
	clientSecret := cfg.GraphClientSecret
	fromAddr := cfg.GraphFromAddress
	recipients := cfg.GraphRecipients
	if req.GraphTenantID != nil {
		tenantID = *req.GraphTenantID
	}
	if req.GraphClientID != nil {
		clientID = *req.GraphClientID
	}
	if req.GraphClientSecret != nil {
		clientSecret = *req.GraphClientSecret
	}
	if req.GraphFromAddress != nil {
		fromAddr = *req.GraphFromAddress
	}
	if req.GraphRecipients != nil {
		recipients = *req.GraphRecipients
	}
	return s.config.SetGraphConfig(tenantID, clientID, clientSecret, fromAddr, recipients)
}

// handleAPIOutputs handles CRUD operations for outputs.
// POST /api/outputs - create
// GET /api/outputs/{id} - read
// PUT /api/outputs/{id} - update
// DELETE /api/outputs/{id} - delete
func (s *Server) handleAPIOutputs(w http.ResponseWriter, r *http.Request) {
	// Extract ID from path: /api/outputs/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/outputs")
	id := strings.TrimPrefix(path, "/")

	switch r.Method {
	case http.MethodPost:
		if id != "" {
			s.writeError(w, http.StatusBadRequest, "POST should not include ID in path")
			return
		}
		s.createOutput(w, r)

	case http.MethodGet:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "GET requires output ID")
			return
		}
		s.getOutput(w, id)

	case http.MethodPut:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "PUT requires output ID")
			return
		}
		s.updateOutput(w, r, id)

	case http.MethodDelete:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "DELETE requires output ID")
			return
		}
		s.deleteOutput(w, id)

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// OutputRequest is the request body for creating/updating outputs.
type OutputRequest struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	StreamID   string `json:"stream_id"`
	Codec      string `json:"codec"`
	MaxRetries int    `json:"max_retries"`
}

func (s *Server) createOutput(w http.ResponseWriter, r *http.Request) {
	var req OutputRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	if req.Host == "" {
		s.writeError(w, http.StatusBadRequest, "host is required")
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		s.writeError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}

	output := &types.Output{
		Enabled:    true, // New outputs start enabled
		Host:       req.Host,
		Port:       req.Port,
		Password:   req.Password,
		StreamID:   req.StreamID,
		Codec:      types.Codec(req.Codec),
		MaxRetries: req.MaxRetries,
	}

	if err := s.config.AddOutput(output); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Start the output if encoder is running
	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StartOutput(output.ID); err != nil {
			slog.Warn("failed to start new output", "output_id", output.ID, "error", err)
		}
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, output)
}

func (s *Server) getOutput(w http.ResponseWriter, id string) {
	output := s.config.Output(id)
	if output == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}
	s.writeJSON(w, http.StatusOK, output)
}

func (s *Server) updateOutput(w http.ResponseWriter, r *http.Request, id string) {
	existing := s.config.Output(id)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}

	var req OutputRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Update fields
	existing.Enabled = req.Enabled
	if req.Host != "" {
		existing.Host = req.Host
	}
	if req.Port > 0 {
		existing.Port = req.Port
	}
	if req.Password != "" {
		existing.Password = req.Password
	}
	if req.StreamID != "" {
		existing.StreamID = req.StreamID
	}
	if req.Codec != "" {
		existing.Codec = types.Codec(req.Codec)
	}
	if req.MaxRetries > 0 {
		existing.MaxRetries = req.MaxRetries
	}

	if err := s.config.UpdateOutput(existing); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Restart output if encoder is running and settings changed
	if s.encoder.State() == types.StateRunning {
		// Stop and restart the output
		if err := s.encoder.StopOutput(id); err != nil {
			slog.Warn("failed to stop output for restart", "output_id", id, "error", err)
		}
		go func() {
			time.Sleep(types.OutputRestartDelay)
			if s.encoder.State() == types.StateRunning {
				if err := s.encoder.StartOutput(id); err != nil {
					slog.Warn("failed to restart output", "output_id", id, "error", err)
				}
			}
		}()
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteOutput(w http.ResponseWriter, id string) {
	if s.config.Output(id) == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}

	// Stop output if running
	if err := s.encoder.StopOutput(id); err != nil {
		slog.Warn("failed to stop output before delete", "output_id", id, "error", err)
	}

	if err := s.config.RemoveOutput(id); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleAPIRecorders handles CRUD operations for recorders.
func (s *Server) handleAPIRecorders(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/recorders")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	id := ""
	action := ""
	if len(parts) > 0 && parts[0] != "" {
		id = parts[0]
	}
	if len(parts) > 1 {
		action = parts[1]
	}

	// Handle actions first
	if action != "" {
		switch action {
		case "start":
			s.startRecorderAPI(w, r, id)
		case "stop":
			s.stopRecorderAPI(w, r, id)
		default:
			s.writeError(w, http.StatusBadRequest, "Unknown action: "+action)
		}
		return
	}

	// Handle test-s3 (no ID required)
	if id == "test-s3" {
		s.testS3API(w, r)
		return
	}

	switch r.Method {
	case http.MethodPost:
		if id != "" {
			s.writeError(w, http.StatusBadRequest, "POST should not include ID in path")
			return
		}
		s.createRecorder(w, r)

	case http.MethodGet:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "GET requires recorder ID")
			return
		}
		s.getRecorder(w, id)

	case http.MethodPut:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "PUT requires recorder ID")
			return
		}
		s.updateRecorder(w, r, id)

	case http.MethodDelete:
		if id == "" {
			s.writeError(w, http.StatusBadRequest, "DELETE requires recorder ID")
			return
		}
		s.deleteRecorder(w, id)

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// RecorderRequest is the request body for creating/updating recorders.
type RecorderRequest struct {
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	Codec             string `json:"codec"`
	RotationMode      string `json:"rotation_mode"`
	StorageMode       string `json:"storage_mode"`
	LocalPath         string `json:"local_path"`
	S3Endpoint        string `json:"s3_endpoint"`
	S3Bucket          string `json:"s3_bucket"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
	RetentionDays     int    `json:"retention_days"`
}

func (s *Server) createRecorder(w http.ResponseWriter, r *http.Request) {
	var req RecorderRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	recorder := &types.Recorder{
		Name:              req.Name,
		Enabled:           true,
		Codec:             types.Codec(req.Codec),
		RotationMode:      types.RotationMode(req.RotationMode),
		StorageMode:       types.StorageMode(req.StorageMode),
		LocalPath:         req.LocalPath,
		S3Endpoint:        req.S3Endpoint,
		S3Bucket:          req.S3Bucket,
		S3AccessKeyID:     req.S3AccessKeyID,
		S3SecretAccessKey: req.S3SecretAccessKey,
		RetentionDays:     req.RetentionDays,
	}

	// AddRecorder saves to config and registers with recording manager
	if err := s.encoder.AddRecorder(recorder); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, recorder)
}

func (s *Server) getRecorder(w http.ResponseWriter, id string) {
	recorder := s.config.Recorder(id)
	if recorder == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}
	s.writeJSON(w, http.StatusOK, recorder)
}

func (s *Server) updateRecorder(w http.ResponseWriter, r *http.Request, id string) {
	existing := s.config.Recorder(id)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	var req RecorderRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Update fields
	existing.Enabled = req.Enabled
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Codec != "" {
		existing.Codec = types.Codec(req.Codec)
	}
	if req.RotationMode != "" {
		existing.RotationMode = types.RotationMode(req.RotationMode)
	}
	if req.StorageMode != "" {
		existing.StorageMode = types.StorageMode(req.StorageMode)
	}
	if req.LocalPath != "" {
		existing.LocalPath = req.LocalPath
	}
	if req.S3Endpoint != "" {
		existing.S3Endpoint = req.S3Endpoint
	}
	if req.S3Bucket != "" {
		existing.S3Bucket = req.S3Bucket
	}
	if req.S3AccessKeyID != "" {
		existing.S3AccessKeyID = req.S3AccessKeyID
	}
	if req.S3SecretAccessKey != "" {
		existing.S3SecretAccessKey = req.S3SecretAccessKey
	}
	if req.RetentionDays > 0 {
		existing.RetentionDays = req.RetentionDays
	}

	// UpdateRecorder saves to config and updates recording manager
	if err := s.encoder.UpdateRecorder(existing); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteRecorder(w http.ResponseWriter, id string) {
	if s.config.Recorder(id) == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}

	// RemoveRecorder stops recording, removes from manager, and removes from config
	if err := s.encoder.RemoveRecorder(id); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) startRecorderAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Start encoder if not running
	if s.encoder.State() == types.StateStopped {
		if err := s.encoder.Start(); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to start encoder: "+err.Error())
			return
		}
	}

	if err := s.encoder.StartRecorder(id); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) stopRecorderAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if err := s.encoder.StopRecorder(id); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// S3TestRequest is the request body for testing S3 connectivity.
type S3TestRequest struct {
	Endpoint  string `json:"s3_endpoint"`
	Bucket    string `json:"s3_bucket"`
	AccessKey string `json:"s3_access_key_id"`
	SecretKey string `json:"s3_secret_access_key"`
}

func (s *Server) testS3API(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req S3TestRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
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

	// Test S3 connection using recording package
	cfg := &types.Recorder{
		S3Endpoint:        req.Endpoint,
		S3Bucket:          req.Bucket,
		S3AccessKeyID:     req.AccessKey,
		S3SecretAccessKey: req.SecretKey,
	}

	if err := recording.TestRecorderS3Connection(cfg); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// Notification test endpoints

// NotificationTestRequest is the request body for testing notifications.
type NotificationTestRequest struct {
	// Webhook
	WebhookURL string `json:"webhook_url,omitempty"`

	// Log
	LogPath string `json:"log_path,omitempty"`

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

func (s *Server) handleAPITestWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req NotificationTestRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	cfg := s.config.Snapshot()
	url := req.WebhookURL
	if url == "" {
		url = cfg.WebhookURL
	}

	if url == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "No webhook URL configured"})
		return
	}

	if err := notify.SendTestWebhook(url, cfg.StationName); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleAPITestLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req NotificationTestRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	path := req.LogPath
	if path == "" {
		path = s.config.Snapshot().LogPath
	}

	if path == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "No log path configured"})
		return
	}

	if err := notify.WriteTestLog(path); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleAPITestEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req NotificationTestRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Use request values or fall back to saved config
	cfg := s.config.Snapshot()

	tenantID := req.GraphTenantID
	clientID := req.GraphClientID
	clientSecret := req.GraphClientSecret
	fromAddress := req.GraphFromAddress
	recipients := req.GraphRecipients

	// Fill in missing values from config
	if tenantID == "" {
		tenantID = cfg.GraphTenantID
	}
	if clientID == "" {
		clientID = cfg.GraphClientID
	}
	if clientSecret == "" {
		clientSecret = cfg.GraphClientSecret
	}
	if fromAddress == "" {
		fromAddress = cfg.GraphFromAddress
	}
	if recipients == "" {
		recipients = cfg.GraphRecipients
	}

	if tenantID == "" || clientID == "" || clientSecret == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "Email not fully configured"})
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
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleAPITestZabbix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req NotificationTestRequest
	if err := s.readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Use request values or fall back to saved config
	cfg := s.config.Snapshot()
	server := req.ZabbixServer
	port := req.ZabbixPort
	host := req.ZabbixHost
	key := req.ZabbixKey

	if server == "" {
		server = cfg.ZabbixServer
	}
	if port == 0 {
		port = cfg.ZabbixPort
	}
	if host == "" {
		host = cfg.ZabbixHost
	}
	if key == "" {
		key = cfg.ZabbixKey
	}

	if server == "" || host == "" || key == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "Zabbix not fully configured"})
		return
	}

	if err := notify.SendTestZabbix(server, port, host, key); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleAPIRegenerateKey generates a new recording API key.
func (s *Server) handleAPIRegenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Generate new API key
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
	s.writeJSON(w, http.StatusOK, map[string]string{
		"api_key": newKey,
	})
}

// handleAPIViewLog returns the silence log entries.
func (s *Server) handleAPIViewLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	logPath := s.config.LogPath()
	if logPath == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   "Log file path not configured",
		})
		return
	}

	entries, err := readSilenceLog(logPath, 100)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"entries": entries,
		"path":    logPath,
	})
}

// readSilenceLog reads the last N entries from the silence log file.
func readSilenceLog(logPath string, maxEntries int) ([]types.SilenceLogEntry, error) {
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return []types.SilenceLogEntry{}, nil
	}
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return []types.SilenceLogEntry{}, nil
	}

	start := max(0, len(lines)-maxEntries)
	lines = lines[start:]

	entries := make([]types.SilenceLogEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry types.SilenceLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			slog.Warn("failed to parse silence log entry", "line", line, "error", err)
			continue
		}
		entries = append(entries, entry)
	}

	// Reverse to show newest first
	slices.Reverse(entries)

	return entries, nil
}

// Placeholder functions for event configs - these need to be implemented
// based on how events are stored in the config

func (s *Server) getWebhookEvents() types.EventConfig {
	// TODO: Implement when event storage is added to config
	return types.EventConfig{SilenceStart: true, SilenceEnd: true, AudioDump: true}
}

func (s *Server) getLogEvents() types.EventConfig {
	return types.EventConfig{SilenceStart: true, SilenceEnd: true, AudioDump: true}
}

func (s *Server) getZabbixEvents() types.EventConfig {
	return types.EventConfig{SilenceStart: true, SilenceEnd: true, AudioDump: true}
}

func (s *Server) getGraphEvents() types.EventConfig {
	return types.EventConfig{SilenceStart: true, SilenceEnd: true, AudioDump: true}
}

