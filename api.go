package main

import (
	"cmp"
	"context"
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

// writeJSON writes a JSON response with the given status code.
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

// writeError writes a JSON error response with the given status code.
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}

// writeMessage writes a JSON response with a message.
func (s *Server) writeMessage(w http.ResponseWriter, message string) {
	s.writeJSON(w, http.StatusOK, map[string]string{"message": message})
}

// writeNoContent writes a 204 No Content response.
func (s *Server) writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// readJSON decodes JSON from the request body into v.
func (s *Server) readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// maxRequestBodySize is the maximum allowed size for JSON request bodies (1MB).
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
// GET /api/config
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

		// Notifications - Log
		LogPath: cfg.LogPath,

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
		Outputs:   cfg.Outputs,
		Recorders: cfg.Recorders,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIDevices returns available audio devices.
// GET /api/devices
func (s *Server) handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"devices": audio.ListDevices(),
	})
}

// SettingsUpdateRequest is the request body for POST /api/settings.
type SettingsUpdateRequest struct {
	// Audio
	AudioInput string `json:"audio_input"`

	// Silence detection
	SilenceThreshold         float64 `json:"silence_threshold"`
	SilenceDurationMs        int64   `json:"silence_duration_ms"`
	SilenceRecoveryMs        int64   `json:"silence_recovery_ms"`
	SilenceDumpEnabled       bool    `json:"silence_dump_enabled"`
	SilenceDumpRetentionDays int     `json:"silence_dump_retention_days"`

	// Webhook
	WebhookURL string `json:"webhook_url"`

	// Log
	LogPath string `json:"log_path"`

	// Zabbix
	ZabbixServer string `json:"zabbix_server"`
	ZabbixPort   int    `json:"zabbix_port"`
	ZabbixHost   string `json:"zabbix_host"`
	ZabbixKey    string `json:"zabbix_key"`

	// Email (Graph)
	GraphTenantID     string `json:"graph_tenant_id"`
	GraphClientID     string `json:"graph_client_id"`
	GraphClientSecret string `json:"graph_client_secret"` // empty = keep existing
	GraphFromAddress  string `json:"graph_from_address"`
	GraphRecipients   string `json:"graph_recipients"`
}

// handleAPISettings updates all settings atomically.
// POST /api/settings
func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[SettingsUpdateRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	audioInputChanged := req.AudioInput != cfg.AudioInput

	// Apply audio settings
	if err := s.config.SetAudioInput(req.AudioInput); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply silence detection settings
	if err := s.config.SetSilenceThreshold(req.SilenceThreshold); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.config.SetSilenceDurationMs(req.SilenceDurationMs); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.config.SetSilenceRecoveryMs(req.SilenceRecoveryMs); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.config.SetSilenceDump(req.SilenceDumpEnabled, req.SilenceDumpRetentionDays); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply notification settings
	if err := s.config.SetWebhookURL(req.WebhookURL); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.config.SetLogPath(req.LogPath); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply Zabbix settings
	if err := s.config.SetZabbixConfig(req.ZabbixServer, req.ZabbixPort, req.ZabbixHost, req.ZabbixKey); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply Graph settings (keep existing secret if empty)
	secret := cmp.Or(req.GraphClientSecret, cfg.GraphClientSecret)
	if err := s.config.SetGraphConfig(req.GraphTenantID, req.GraphClientID, secret, req.GraphFromAddress, req.GraphRecipients); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Update encoder's silence config
	s.encoder.UpdateSilenceConfig()

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

// Output API endpoints

// handleListOutputs returns all configured outputs.
// GET /api/outputs
func (s *Server) handleListOutputs(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	s.writeJSON(w, http.StatusOK, cfg.Outputs)
}

// handleGetOutput returns a single output by ID.
// GET /api/outputs/{id}
func (s *Server) handleGetOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	output := s.config.Output(id)
	if output == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}
	s.writeJSON(w, http.StatusOK, output)
}

// OutputRequest is the request body for creating/updating outputs.
type OutputRequest struct {
	Enabled    bool        `json:"enabled"`
	Host       string      `json:"host"`
	Port       int         `json:"port"`
	Password   string      `json:"password"`
	StreamID   string      `json:"stream_id"`
	Codec      types.Codec `json:"codec"`
	MaxRetries int         `json:"max_retries"`
}

// handleCreateOutput creates a new output.
// POST /api/outputs
func (s *Server) handleCreateOutput(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[OutputRequest](s, w, r)
	if !ok {
		return
	}

	output := &types.Output{
		Enabled:    true,
		Host:       req.Host,
		Port:       req.Port,
		Password:   req.Password,
		StreamID:   req.StreamID,
		Codec:      req.Codec, // Already validated by UnmarshalJSON
		MaxRetries: req.MaxRetries,
	}

	if err := s.config.AddOutput(output); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.encoder.State() == types.StateRunning {
		if err := s.encoder.StartOutput(output.ID); err != nil {
			slog.Warn("failed to start new output", "output_id", output.ID, "error", err)
		}
	}

	s.broadcastConfigChanged()
	s.writeJSON(w, http.StatusCreated, output)
}

// handleUpdateOutput replaces an output by ID.
// PUT /api/outputs/{id}
func (s *Server) handleUpdateOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing := s.config.Output(id)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}

	req, ok := parseJSON[OutputRequest](s, w, r)
	if !ok {
		return
	}

	// Full replacement - preserve only ID and CreatedAt
	// For password: empty string means "keep existing" (not sent from frontend for security)
	updated := &types.Output{
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

	if err := s.config.UpdateOutput(updated); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Restart output if encoder is running
	if s.encoder.State() == types.StateRunning {
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
	s.writeJSON(w, http.StatusOK, updated)
}

// handleDeleteOutput deletes an output by ID.
// DELETE /api/outputs/{id}
func (s *Server) handleDeleteOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.config.Output(id) == nil {
		s.writeError(w, http.StatusNotFound, "Output not found")
		return
	}

	if err := s.encoder.StopOutput(id); err != nil {
		slog.Warn("failed to stop output before delete", "output_id", id, "error", err)
	}

	if err := s.config.RemoveOutput(id); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastConfigChanged()
	s.writeNoContent(w)
}

// Recorder API endpoints

// handleListRecorders returns all configured recorders.
// GET /api/recorders
func (s *Server) handleListRecorders(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	s.writeJSON(w, http.StatusOK, cfg.Recorders)
}

// handleGetRecorder returns a single recorder by ID.
// GET /api/recorders/{id}
func (s *Server) handleGetRecorder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recorder := s.config.Recorder(id)
	if recorder == nil {
		s.writeError(w, http.StatusNotFound, "Recorder not found")
		return
	}
	s.writeJSON(w, http.StatusOK, recorder)
}

// RecorderRequest is the request body for creating/updating recorders.
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
// POST /api/recorders
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
// PUT /api/recorders/{id}
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
// DELETE /api/recorders/{id}
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
// POST /api/recorders/{id}/start
// POST /api/recorders/{id}/stop
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

// S3TestRequest is the request body for testing S3 connectivity.
type S3TestRequest struct {
	Endpoint  string `json:"s3_endpoint"`
	Bucket    string `json:"s3_bucket"`
	AccessKey string `json:"s3_access_key_id"`
	SecretKey string `json:"s3_secret_access_key"`
}

// handleTestS3 tests S3 connectivity.
// POST /api/recorders/test-s3
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

// handleAPITestWebhook tests webhook notification connectivity.
func (s *Server) handleAPITestWebhook(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	cfg := s.config.Snapshot()
	url := cmp.Or(req.WebhookURL, cfg.WebhookURL)

	if url == "" {
		s.writeError(w, http.StatusBadRequest, "No webhook URL configured")
		return
	}

	if err := notify.SendTestWebhook(url, cfg.StationName); err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.writeMessage(w, "Webhook test sent")
}

// handleAPITestLog tests log file notification.
func (s *Server) handleAPITestLog(w http.ResponseWriter, r *http.Request) {
	req, ok := parseJSON[NotificationTestRequest](s, w, r)
	if !ok {
		return
	}

	path := cmp.Or(req.LogPath, s.config.Snapshot().LogPath)

	if path == "" {
		s.writeError(w, http.StatusBadRequest, "No log path configured")
		return
	}

	if err := notify.WriteTestLog(path); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeMessage(w, "Test log entry written")
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

// handleAPIViewLog returns the silence log entries.
func (s *Server) handleAPIViewLog(w http.ResponseWriter, r *http.Request) {
	logPath := s.config.LogPath()
	if logPath == "" {
		s.writeError(w, http.StatusBadRequest, "Log file path not configured")
		return
	}

	entries, err := readSilenceLog(logPath, 100)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
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

	content := strings.TrimSpace(string(data))
	if content == "" {
		return []types.SilenceLogEntry{}, nil
	}

	ring := make([]types.SilenceLogEntry, maxEntries)
	count := 0

	for line := range strings.Lines(content) {
		if line == "" {
			continue
		}
		var entry types.SilenceLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			slog.Warn("failed to parse silence log entry", "line", line, "error", err)
			continue
		}
		ring[count%maxEntries] = entry
		count++
	}

	if count == 0 {
		return []types.SilenceLogEntry{}, nil
	}

	size := min(count, maxEntries)
	entries := make([]types.SilenceLogEntry, size)
	start := count - size
	for i := range size {
		entries[i] = ring[(start+i)%maxEntries]
	}

	slices.Reverse(entries)

	return entries, nil
}
