package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestDerefReturnsValueWhenPresentFallbackWhenNil(t *testing.T) {
	t.Parallel()
	empty := ""
	value := "explicit"
	if got := deref((*string)(nil), "saved"); got != "saved" {
		t.Fatalf("deref(nil, saved) = %q, want %q", got, "saved")
	}
	if got := deref(&empty, "saved"); got != "" {
		t.Fatalf("deref(&\"\", saved) = %q, want empty string", got)
	}
	if got := deref(&value, "saved"); got != "explicit" {
		t.Fatalf("deref(&explicit, saved) = %q, want %q", got, "explicit")
	}
}
func TestValidateRecorderLocalPathCreatesWritableDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "archive")
	recorder := &types.Recorder{
		StorageMode: types.StorageLocal,
		LocalPath:   path,
	}
	if err := validateRecorderLocalPath(recorder); err != nil {
		t.Fatalf("validateRecorderLocalPath() error = %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("local path stat = (%v, %v), want directory", info, err)
	}
}
func TestValidateRecorderLocalPathRejectsTraversal(t *testing.T) {
	t.Parallel()
	recorder := &types.Recorder{
		StorageMode: types.StorageBoth,
		LocalPath:   t.TempDir() + "/../archive",
	}
	if err := validateRecorderLocalPath(recorder); err == nil {
		t.Fatal("validateRecorderLocalPath() error = nil, want error")
	}
}
func TestBuildReadyResponseReady(t *testing.T) {
	t.Parallel()
	input := readyFixture()
	resp, status := buildReadyResponse(&input)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if resp.Status != "ready" {
		t.Fatalf("ready status = %q, want ready", resp.Status)
	}
	for name, component := range resp.Components {
		if !component.OK {
			t.Fatalf("component %q not ready: %+v", name, component)
		}
	}
}
func TestBuildReadyResponseFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		component string
		mutate    func(*readyInputs)
	}{
		{
			name:      "ffmpeg missing",
			component: "process",
			mutate: func(in *readyInputs) {
				in.ffmpegAvailable = false
			},
		},
		{
			name:      "encoder stopped",
			component: "process",
			mutate: func(in *readyInputs) {
				in.encoderStatus.State = types.StateStopped
			},
		},
		{
			name:      "stream not stable",
			component: "streams",
			mutate: func(in *readyInputs) {
				status := in.streamStatuses["stream-1"]
				status.Stable = false
				in.streamStatuses["stream-1"] = status
			},
		},
		{
			name:      "active silence",
			component: "silence",
			mutate: func(in *readyInputs) {
				in.audioLevels.SilenceLevel = audio.SilenceLevelActive
			},
		},
		{
			name:      "active channel imbalance",
			component: "channel_imbalance",
			mutate: func(in *readyInputs) {
				in.audioLevels.ChannelImbalanceLevel = audio.ImbalanceLevelActive
			},
		},
		{
			name:      "hourly recorder stopped",
			component: "recorders",
			mutate: func(in *readyInputs) {
				status := in.recorderStatuses["recorder-1"]
				status.State = types.ProcessStopped
				in.recorderStatuses["recorder-1"] = status
			},
		},
		{
			name:      "pending uploads",
			component: "uploads",
			mutate: func(in *readyInputs) {
				in.pendingUploads = 1
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := readyFixture()
			tt.mutate(&input)
			resp, status := buildReadyResponse(&input)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
			}
			if resp.Status != "not_ready" {
				t.Fatalf("ready status = %q, want not_ready", resp.Status)
			}
			if resp.Components[tt.component].OK {
				t.Fatalf("component %q OK = true, want false; response = %+v", tt.component, resp)
			}
		})
	}
}
func TestBuildReadyResponseAllowsStoppedOnDemandRecorder(t *testing.T) {
	t.Parallel()
	input := readyFixture()
	input.recorders = []types.Recorder{
		{
			ID:            "recorder-ondemand",
			Enabled:       true,
			RecordingMode: types.RecordingOnDemand,
		},
	}
	input.recorderStatuses = map[string]types.ProcessStatus{
		"recorder-ondemand": {State: types.ProcessStopped},
	}
	resp, status := buildReadyResponse(&input)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; response = %+v", status, http.StatusOK, resp)
	}
	if !resp.Components["recorders"].OK {
		t.Fatalf("recorders component = %+v, want OK", resp.Components["recorders"])
	}
}
func TestBuildReadyResponseIgnoresListenerStreams(t *testing.T) {
	t.Parallel()
	input := readyFixture()
	input.streams = []types.Stream{
		{
			ID:      "listener-1",
			Enabled: true,
			Mode:    types.StreamModeListener,
		},
	}
	input.streamStatuses = map[string]types.ProcessStatus{
		"listener-1": {
			State:  types.ProcessRunning,
			Stable: false,
		},
	}
	resp, status := buildReadyResponse(&input)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; response = %+v", status, http.StatusOK, resp)
	}
	streams := resp.Components["streams"]
	if !streams.OK {
		t.Fatalf("streams component = %+v, want OK for listener-only config", streams)
	}
	if got := streams.Details["production_monitored"]; got != 0 {
		t.Fatalf("production_monitored = %v, want 0", got)
	}
}

func TestHandleAPIEventsDecoratesClassificationAndKeepsPagination(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	writeAPIEvents(t, logPath, []eventlog.Event{
		{
			Timestamp: time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC),
			Type:      eventlog.StreamStarted,
			Message:   "Connecting to srt://example",
		},
		{
			Timestamp: time.Date(2026, 6, 21, 10, 1, 0, 0, time.UTC),
			Type:      eventlog.UploadFailed,
		},
		{
			Timestamp: time.Date(2026, 6, 21, 10, 2, 0, 0, time.UTC),
			Type:      eventlog.UploadCompleted,
		},
	})

	server := freshServer(t)
	rec := runJSONHandler(
		t,
		func(w http.ResponseWriter, r *http.Request) {
			server.handleAPIEventsFromPath(w, r, logPath)
		},
		http.MethodGet,
		"/api/events?limit=1&type=recorder",
		"",
	)
	assertStatus(t, rec, http.StatusOK)
	firstPage := decodeJSON[eventsResponseForTest](t, rec.Body.Bytes())
	if !firstPage.HasMore {
		t.Fatal("has_more = false, want true")
	}
	if len(firstPage.Events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(firstPage.Events))
	}
	assertEventView(
		t,
		firstPage.Events[0],
		eventlog.UploadCompleted,
		eventlog.SeveritySuccess,
		eventlog.CategoryRecorder,
		eventlog.ReasonRoutine,
	)

	rec = runJSONHandler(
		t,
		func(w http.ResponseWriter, r *http.Request) {
			server.handleAPIEventsFromPath(w, r, logPath)
		},
		http.MethodGet,
		"/api/events?limit=1&offset=1&type=recorder",
		"",
	)
	assertStatus(t, rec, http.StatusOK)
	secondPage := decodeJSON[eventsResponseForTest](t, rec.Body.Bytes())
	if secondPage.HasMore {
		t.Fatal("has_more = true, want false")
	}
	if len(secondPage.Events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(secondPage.Events))
	}
	assertEventView(
		t,
		secondPage.Events[0],
		eventlog.UploadFailed,
		eventlog.SeverityError,
		eventlog.CategoryRecorder,
		eventlog.ReasonProblem,
	)
}

func readyFixture() readyInputs {
	return readyInputs{
		ffmpegAvailable:    true,
		recordingAvailable: true,
		encoderStatus: types.EncoderStatus{
			State: types.StateRunning,
		},
		streams: []types.Stream{
			{
				ID:      "stream-1",
				Enabled: true,
			},
		},
		streamStatuses: map[string]types.ProcessStatus{
			"stream-1": {
				State:  types.ProcessRunning,
				Stable: true,
			},
		},
		recorders: []types.Recorder{
			{
				ID:            "recorder-1",
				Enabled:       true,
				RecordingMode: types.RecordingHourly,
			},
		},
		recorderStatuses: map[string]types.ProcessStatus{
			"recorder-1": {
				State: types.ProcessRunning,
			},
		},
		audioLevels:    audio.AudioLevels{},
		pendingUploads: 0,
	}
}

type eventViewForTest struct {
	Type     eventlog.EventType `json:"type"`
	Severity eventlog.Severity  `json:"severity"`
	Category eventlog.Category  `json:"category"`
	Reason   eventlog.Reason    `json:"reason"`
}

type eventsResponseForTest struct {
	Events  []eventViewForTest `json:"events"`
	HasMore bool               `json:"has_more"`
}

func writeAPIEvents(t *testing.T, path string, events []eventlog.Event) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // Test path is under t.TempDir.
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	enc := json.NewEncoder(file)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			_ = file.Close()
			t.Fatalf("encode event: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
}

func assertEventView(
	t *testing.T,
	got eventViewForTest,
	wantType eventlog.EventType,
	wantSeverity eventlog.Severity,
	wantCategory eventlog.Category,
	wantReason eventlog.Reason,
) {
	t.Helper()
	if got.Type != wantType {
		t.Fatalf("type = %q, want %q", got.Type, wantType)
	}
	if got.Severity != wantSeverity {
		t.Fatalf("severity = %q, want %q", got.Severity, wantSeverity)
	}
	if got.Category != wantCategory {
		t.Fatalf("category = %q, want %q", got.Category, wantCategory)
	}
	if got.Reason != wantReason {
		t.Fatalf("reason = %q, want %q", got.Reason, wantReason)
	}
}

func healthFixture() healthInputs {
	return healthInputs{
		ffmpegAvailable: true,
		encoderStatus:   types.EncoderStatus{State: types.StateRunning},
		audioLevels:     audio.AudioLevels{},
	}
}

func TestBuildHealthResponseAudioConditionsAreInformational(t *testing.T) {
	t.Parallel()
	t.Run("active channel imbalance stays healthy", func(t *testing.T) {
		t.Parallel()
		in := healthFixture()
		in.audioLevels.ChannelImbalanceLevel = audio.ImbalanceLevelActive
		resp, status := buildHealthResponse(&in)
		if status != http.StatusOK || resp.Status != "healthy" {
			t.Fatalf("status = %d/%q, want 200/healthy", status, resp.Status)
		}
		if !resp.ChannelImbalanceDetected {
			t.Fatal("ChannelImbalanceDetected = false, want true (informational field must still be reported)")
		}
	})
	t.Run("active silence stays healthy", func(t *testing.T) {
		t.Parallel()
		in := healthFixture()
		in.audioLevels.SilenceLevel = audio.SilenceLevelActive
		resp, status := buildHealthResponse(&in)
		if status != http.StatusOK || resp.Status != "healthy" {
			t.Fatalf("status = %d/%q, want 200/healthy", status, resp.Status)
		}
		if !resp.SilenceDetected {
			t.Fatal("SilenceDetected = false, want true")
		}
	})
}

func TestBuildHealthResponseUnhealthyConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*healthInputs)
	}{
		{"ffmpeg unavailable", func(in *healthInputs) { in.ffmpegAvailable = false }},
		{"encoder not running", func(in *healthInputs) { in.encoderStatus.State = types.StateStopped }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := healthFixture()
			tt.mutate(&in)
			resp, status := buildHealthResponse(&in)
			if status != http.StatusServiceUnavailable || resp.Status != "unhealthy" {
				t.Fatalf("status = %d/%q, want 503/unhealthy", status, resp.Status)
			}
		})
	}
}

func freshServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return &Server{config: cfg}
}
func decodeError(t *testing.T, body []byte) string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response JSON = %q, error = %v", string(body), err)
	}
	return payload["error"]
}
func assertNotContains(t *testing.T, body []byte, forbidden ...string) {
	t.Helper()
	text := string(body)
	for _, s := range forbidden {
		if strings.Contains(text, s) {
			t.Fatalf("response body leaks %q: %s", s, text)
		}
	}
}
func assertContains(t *testing.T, body []byte, want ...string) {
	t.Helper()
	text := string(body)
	for _, s := range want {
		if !strings.Contains(text, s) {
			t.Fatalf("response body = %s, want to contain %q", text, s)
		}
	}
}
func runJSONHandler(
	t *testing.T,
	handler func(http.ResponseWriter, *http.Request),
	method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, want, rec.Body.String())
	}
}
func assertErrorEqual(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := decodeError(t, rec.Body.Bytes()); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
func assertErrorContains(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := decodeError(t, rec.Body.Bytes()); !strings.Contains(got, want) {
		t.Fatalf("error = %q, want substring %q", got, want)
	}
}
func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var got T
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response JSON = %q, error = %v", string(body), err)
	}
	return got
}
func TestNotificationTestEndpointPreflights(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantError  string
		contains   bool
		call       func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:       "webhook empty url",
			path:       "/api/notifications/test/webhook",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "No webhook URL configured",
			call:       (*Server).handleAPITestWebhook,
		},
		{
			name:       "email missing secret",
			path:       "/api/notifications/test/email",
			body:       `{"graph_tenant_id":"tenant","graph_client_id":"client"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Email not fully configured",
			call:       (*Server).handleAPITestEmail,
		},
		{
			name:       "zabbix out-of-range port reaches runtime",
			path:       "/api/notifications/test/zabbix",
			body:       `{"zabbix_server":"zabbix.example.com","zabbix_port":70000,"zabbix_host":"encoder-01","zabbix_silence_key":"silence"}`,
			wantStatus: http.StatusBadGateway,
			wantError:  "not fully configured",
			contains:   true,
			call:       (*Server).handleAPITestZabbix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := freshServer(t)
			rec := runJSONHandler(t, func(w http.ResponseWriter, r *http.Request) {
				tt.call(s, w, r)
			}, http.MethodPost, tt.path, tt.body)
			assertStatus(t, rec, tt.wantStatus)
			if tt.contains {
				assertErrorContains(t, rec, tt.wantError)
			} else {
				assertErrorEqual(t, rec, tt.wantError)
			}
		})
	}
}

func TestHandleTestS3MissingFieldReturnsBadRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing bucket reported first",
			body:    `{}`,
			wantErr: "s3_bucket is required",
		},
		{
			name:    "bucket set, access key missing",
			body:    `{"s3_bucket": "b"}`,
			wantErr: "s3_access_key_id is required",
		},
		{
			name:    "bucket+access set, secret missing",
			body:    `{"s3_bucket": "b", "s3_access_key_id": "k"}`,
			wantErr: "s3_secret_access_key is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := freshServer(t)
			rec := runJSONHandler(t, s.handleTestS3, http.MethodPost, "/api/recorders/test-s3", tt.body)
			assertStatus(t, rec, http.StatusBadRequest)
			assertErrorEqual(t, rec, tt.wantErr)
		})
	}
}
func TestResolveS3TestSecret(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	tests := []struct {
		name      string
		req       S3TestRequest
		want      string
		wantFound bool
	}{
		{
			name: "request secret wins",
			req: S3TestRequest{
				RecorderID: fixture.recorderID,
				SecretKey:  "typed-secret",
			},
			want:      "typed-secret",
			wantFound: true,
		},
		{
			name: "empty secret falls back to saved recorder secret",
			req: S3TestRequest{
				RecorderID: fixture.recorderID,
			},
			want:      fixture.s3Secret,
			wantFound: true,
		},
		{
			name:      "empty secret without recorder remains empty",
			req:       S3TestRequest{},
			want:      "",
			wantFound: true,
		},
		{
			name: "missing fallback recorder reports not found",
			req: S3TestRequest{
				RecorderID: "recorder-missing",
			},
			want:      "",
			wantFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := fixture.server.resolveS3TestSecret(&tt.req)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if got != tt.want {
				t.Fatalf("secret = %q, want %q", got, tt.want)
			}
		})
	}
}
func TestHandleTestS3MissingFallbackRecorderReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := freshServer(t)
	body := `{"recorder_id":"recorder-missing","s3_bucket":"b","s3_access_key_id":"k"}`
	rec := runJSONHandler(t, s.handleTestS3, http.MethodPost, "/api/recorders/test-s3", body)
	assertStatus(t, rec, http.StatusNotFound)
	assertErrorEqual(t, rec, "Recorder not found")
}

// seededServer applies an update with valid defaults for unrelated required settings.
func seededServer(t *testing.T, upd *config.SettingsUpdate) *Server {
	t.Helper()
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if upd.SilenceThreshold == 0 {
		upd.SilenceThreshold = -40
	}
	if upd.SilenceDurationMs == 0 {
		upd.SilenceDurationMs = 15000
	}
	if upd.SilenceRecoveryMs == 0 {
		upd.SilenceRecoveryMs = 5000
	}
	if upd.PeakHoldMs == 0 {
		upd.PeakHoldMs = 1500
	}
	if upd.ChannelImbalanceThreshold == 0 {
		upd.ChannelImbalanceThreshold = 12
	}
	if upd.ChannelImbalanceDurationMs == 0 {
		upd.ChannelImbalanceDurationMs = 15000
	}
	if upd.ChannelImbalanceRecoveryMs == 0 {
		upd.ChannelImbalanceRecoveryMs = 5000
	}
	if err := cfg.ApplySettings(upd); err != nil {
		t.Fatalf("ApplySettings() error = %v", err)
	}
	return &Server{config: cfg}
}

// seededGraphSettings returns a saved Graph config that reaches runtime validation.
func seededGraphSettings() *config.SettingsUpdate {
	return &config.SettingsUpdate{
		GraphTenantID:     "tenant-not-a-guid",
		GraphClientID:     "client-not-a-guid",
		GraphClientSecret: "saved-secret",
		GraphFromAddress:  "from@example.com",
		GraphRecipients:   "to@example.com",
	}
}

// seededZabbixSettings returns a complete saved Zabbix config for preservation tests.
func seededZabbixSettings() *config.SettingsUpdate {
	return &config.SettingsUpdate{
		ZabbixServer:     "zabbix.example.com",
		ZabbixPort:       10051,
		ZabbixHost:       "encoder-01",
		ZabbixSilenceKey: "silence",
	}
}

type sensitiveFixture struct {
	server          *Server
	streamID        string
	recorderID      string
	streamPassword  string
	s3Secret        string
	webhookURL      string
	recordingAPIKey string
}

func seededSensitiveServer(t *testing.T) sensitiveFixture {
	t.Helper()
	s := freshServer(t)
	fixture := sensitiveFixture{ //nolint:gosec // G101: Intentional secret-shaped test fixture values verify redaction.
		server:          s,
		streamPassword:  "srt-secret-269",
		s3Secret:        "s3-secret-269",
		webhookURL:      "https://hooks.example.com/services/token-269",
		recordingAPIKey: "recording-key-269",
	}
	upd := validBaselineSettings(s.config)
	upd.WebhookURL = fixture.webhookURL
	if err := s.config.ApplySettings(upd); err != nil {
		t.Fatalf("ApplySettings() error = %v", err)
	}
	if err := s.config.SetRecordingAPIKey(fixture.recordingAPIKey); err != nil {
		t.Fatalf("SetRecordingAPIKey() error = %v", err)
	}
	stream := &types.Stream{
		Host:       "stream.example.com",
		Port:       9000,
		Password:   fixture.streamPassword,
		StreamID:   "studio",
		Codec:      types.CodecMP3,
		Bitrate:    128,
		MaxRetries: 3,
	}
	if err := s.config.AddStream(stream); err != nil {
		t.Fatalf("AddStream() error = %v", err)
	}
	fixture.streamID = stream.ID
	recorder := &types.Recorder{
		Name:              "S3 Recorder",
		Codec:             types.CodecMP3,
		Bitrate:           128,
		RecordingMode:     types.RecordingHourly,
		StorageMode:       types.StorageS3,
		S3Bucket:          "recordings",
		S3AccessKeyID:     "access-key-269",
		S3SecretAccessKey: fixture.s3Secret,
	}
	if err := s.config.AddRecorder(recorder); err != nil {
		t.Fatalf("AddRecorder() error = %v", err)
	}
	fixture.recorderID = recorder.ID
	return fixture
}

func TestHandleAPIConfigRedactsStoredSecrets(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	rec := runJSONHandler(t, fixture.server.handleAPIConfig, http.MethodGet, "/api/config", "")
	assertStatus(t, rec, http.StatusOK)
	body := rec.Body.Bytes()
	assertNotContains(t, body,
		fixture.streamPassword,
		fixture.s3Secret,
		fixture.webhookURL,
		fixture.recordingAPIKey,
		`"password":"`,
		`"s3_secret_access_key":"`,
	)
	assertContains(t, body,
		`"webhook_has_url":true`,
		`"recording_has_api_key":true`,
		`"has_password":true`,
		`"has_s3_secret":true`,
	)
	resp := decodeJSON[types.APIConfigResponse](t, body)
	if !resp.WebhookHasURL {
		t.Fatalf("WebhookHasURL = false, want true")
	}
	if !resp.RecordingHasAPIKey {
		t.Fatalf("RecordingHasAPIKey = false, want true")
	}
	if len(resp.Streams) != 1 || !resp.Streams[0].HasPassword {
		t.Fatalf("Streams = %+v, want one redacted stream with HasPassword=true", resp.Streams)
	}
	if len(resp.Recorders) != 1 || !resp.Recorders[0].HasS3Secret {
		t.Fatalf("Recorders = %+v, want one redacted recorder with HasS3Secret=true", resp.Recorders)
	}
}

func TestHandleAPIConfigIncludesChannelImbalance(t *testing.T) {
	t.Parallel()
	s := freshServer(t)
	rec := runJSONHandler(t, s.handleAPIConfig, http.MethodGet, "/api/config", "")
	assertStatus(t, rec, http.StatusOK)
	assertContains(t, rec.Body.Bytes(),
		`"channel_imbalance_threshold":`,
		`"channel_imbalance_duration_ms":`,
		`"channel_imbalance_recovery_ms":`,
	)
	resp := decodeJSON[types.APIConfigResponse](t, rec.Body.Bytes())
	if resp.ChannelImbalanceThreshold != config.DefaultChannelImbalanceThreshold {
		t.Fatalf("ChannelImbalanceThreshold = %v, want %v", resp.ChannelImbalanceThreshold, config.DefaultChannelImbalanceThreshold)
	}
	if resp.ChannelImbalanceDurationMs != config.DefaultChannelImbalanceDurationMs {
		t.Fatalf("ChannelImbalanceDurationMs = %d, want %d", resp.ChannelImbalanceDurationMs, config.DefaultChannelImbalanceDurationMs)
	}
	if resp.ChannelImbalanceRecoveryMs != config.DefaultChannelImbalanceRecoveryMs {
		t.Fatalf("ChannelImbalanceRecoveryMs = %d, want %d", resp.ChannelImbalanceRecoveryMs, config.DefaultChannelImbalanceRecoveryMs)
	}
}

func TestApplyWithPreserveRoundTripsChannelImbalance(t *testing.T) {
	t.Parallel()
	s := freshServer(t)
	upd := validBaselineSettings(s.config)
	upd.ChannelImbalanceThreshold = 18
	upd.ChannelImbalanceDurationMs = 20000
	upd.ChannelImbalanceRecoveryMs = 4000
	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.ChannelImbalanceThreshold != 18 {
		t.Fatalf("ChannelImbalanceThreshold = %v, want 18", snap.ChannelImbalanceThreshold)
	}
	if snap.ChannelImbalanceDurationMs != 20000 {
		t.Fatalf("ChannelImbalanceDurationMs = %d, want 20000", snap.ChannelImbalanceDurationMs)
	}
	if snap.ChannelImbalanceRecoveryMs != 4000 {
		t.Fatalf("ChannelImbalanceRecoveryMs = %d, want 4000", snap.ChannelImbalanceRecoveryMs)
	}
}
func TestHandleStreamEndpointsRedactPassword(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	for _, tt := range []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{
			name: "list",
			run:  fixture.server.handleListStreams,
			req:  httptest.NewRequest(http.MethodGet, "/api/streams", http.NoBody),
		},
		{
			name: "get",
			run:  fixture.server.handleGetStream,
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/api/streams/"+fixture.streamID, http.NoBody)
				req.SetPathValue("id", fixture.streamID)
				return req
			}(),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.run(rec, tt.req)
			assertStatus(t, rec, http.StatusOK)
			assertNotContains(t, rec.Body.Bytes(), fixture.streamPassword, `"password":"`)
			assertContains(t, rec.Body.Bytes(), `"has_password":true`)
		})
	}
}
func TestHandleCreateStreamListenerDefaults(t *testing.T) {
	t.Parallel()
	s := freshServer(t)
	s.encoder = &encoder.Encoder{}
	rec := runJSONHandler(t, s.handleCreateStream, http.MethodPost, "/api/streams", `{
		"mode": "listener",
		"host": "",
		"port": 9000,
		"max_retries": 3
	}`)
	assertStatus(t, rec, http.StatusCreated)
	resp := decodeJSON[types.StreamResponse](t, rec.Body.Bytes())
	if resp.Mode != types.StreamModeListener {
		t.Fatalf("mode = %q, want listener", resp.Mode)
	}
	if resp.Host != types.DefaultListenerBindHost {
		t.Fatalf("host = %q, want %q", resp.Host, types.DefaultListenerBindHost)
	}
	if resp.Codec != types.CodecMP3 {
		t.Fatalf("codec = %q, want mp3", resp.Codec)
	}
	if resp.StreamID != "" {
		t.Fatalf("stream_id = %q, want empty for listener", resp.StreamID)
	}
}
func TestHandleRecorderEndpointsRedactS3Secret(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	for _, tt := range []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{
			name: "list",
			run:  fixture.server.handleListRecorders,
			req:  httptest.NewRequest(http.MethodGet, "/api/recorders", http.NoBody),
		},
		{
			name: "get",
			run:  fixture.server.handleGetRecorder,
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/api/recorders/"+fixture.recorderID, http.NoBody)
				req.SetPathValue("id", fixture.recorderID)
				return req
			}(),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.run(rec, tt.req)
			assertStatus(t, rec, http.StatusOK)
			assertNotContains(t, rec.Body.Bytes(), fixture.s3Secret, `"s3_secret_access_key":"`)
			assertContains(t, rec.Body.Bytes(), `"has_s3_secret":true`)
		})
	}
}
func TestRedactionHelpersOmitSecretFields(t *testing.T) {
	t.Parallel()
	streamBody, err := json.Marshal(redactStream(&types.Stream{
		Password: "helper-stream-secret",
		Mode:     types.StreamModeListener,
	}))
	if err != nil {
		t.Fatalf("marshal redacted stream: %v", err)
	}
	assertNotContains(t, streamBody, "helper-stream-secret", `"password":"`)
	assertContains(t, streamBody, `"has_password":true`, `"mode":"listener"`)
	recorderBody, err := json.Marshal(redactRecorder(&types.Recorder{
		S3SecretAccessKey: "helper-recorder-secret",
	}))
	if err != nil {
		t.Fatalf("marshal redacted recorder: %v", err)
	}
	assertNotContains(t, recorderBody, "helper-recorder-secret", `"s3_secret_access_key":"`)
	assertContains(t, recorderBody, `"has_s3_secret":true`)
}
func TestPreserveSecretKeepReplaceClearConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     string
		clear     bool
		want      string
		wantError string
	}{
		{
			name: "keep when empty",
			want: "saved-secret",
		},
		{
			name:  "replace when set",
			value: "new-secret",
			want:  "new-secret",
		},
		{
			name:  "clear when flagged",
			clear: true,
		},
		{
			name:      "conflict when clear and set",
			value:     "new-secret",
			clear:     true,
			wantError: "clear_secret: conflicts with non-empty secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := preserveSecret(tt.value, "saved-secret", tt.clear, "clear_secret", "secret")
			if tt.wantError != "" {
				if err == nil || err.Error() != tt.wantError {
					t.Fatalf("preserveSecret() error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("preserveSecret() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("preserveSecret() = %q, want %q", got, tt.want)
			}
		})
	}
}
func streamUpdateBody(t *testing.T, password string, clearPassword bool) string {
	t.Helper()
	req := StreamRequest{
		Enabled:       true,
		Host:          "stream.example.com",
		Port:          9000,
		Password:      password,
		ClearPassword: clearPassword,
		StreamID:      "studio",
		Codec:         types.CodecMP3,
		Bitrate:       128,
		MaxRetries:    3,
	}
	body, err := json.Marshal(req) //nolint:gosec // G117: Test marshals the SRT password field.
	if err != nil {
		t.Fatalf("marshal StreamRequest: %v", err)
	}
	return string(body)
}
func putStream(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/streams/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleUpdateStream(rec, req)
	return rec
}
func putRecorder(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/recorders/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleUpdateRecorder(rec, req)
	return rec
}
func TestHandleUpdateStreamPasswordKeepReplaceClear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		password    string
		clear       bool
		wantSecret  string
		wantHasFlag string
	}{
		{ //nolint:gosec // G101: Test fixture, not a real credential.
			name:        "keep when empty",
			wantSecret:  "srt-secret-269",
			wantHasFlag: `"has_password":true`,
		},
		{ //nolint:gosec // G101: Test fixture, not a real credential.
			name:        "replace when set",
			password:    "new-srt-secret",
			wantSecret:  "new-srt-secret",
			wantHasFlag: `"has_password":true`,
		},
		{
			name:        "clear when flagged",
			clear:       true,
			wantHasFlag: `"has_password":false`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := seededSensitiveServer(t)
			fixture.server.encoder = &encoder.Encoder{}
			rec := putStream(t, fixture.server, fixture.streamID, streamUpdateBody(t, tt.password, tt.clear))
			assertStatus(t, rec, http.StatusOK)
			stream := fixture.server.config.Stream(fixture.streamID)
			if stream == nil {
				t.Fatal("updated stream not found")
			}
			if stream.Password != tt.wantSecret {
				t.Fatalf("stream password = %q, want %q", stream.Password, tt.wantSecret)
			}
			forbidden := []string{fixture.streamPassword, `"password":"`}
			if tt.password != "" {
				forbidden = append(forbidden, tt.password)
			}
			assertNotContains(t, rec.Body.Bytes(), forbidden...)
			assertContains(t, rec.Body.Bytes(), tt.wantHasFlag)
		})
	}
}
func TestHandleUpdateStreamClearPasswordConflictsWithNewValue(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	fixture.server.encoder = &encoder.Encoder{}
	rec := putStream(t, fixture.server, fixture.streamID, streamUpdateBody(t, "new-srt-secret", true))
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorEqual(t, rec, "clear_password: conflicts with non-empty password")
	stream := fixture.server.config.Stream(fixture.streamID)
	if stream == nil {
		t.Fatal("stream not found")
	}
	if stream.Password != fixture.streamPassword {
		t.Fatalf("stream password = %q, want unchanged %q", stream.Password, fixture.streamPassword)
	}
}
func recorderUpdateBody(t *testing.T, secret string, clearS3Secret bool) string {
	t.Helper()
	req := RecorderRequest{
		Name:              "S3 Recorder",
		Enabled:           true,
		Codec:             types.CodecMP3,
		Bitrate:           128,
		RecordingMode:     types.RecordingHourly,
		StorageMode:       types.StorageS3,
		S3Bucket:          "recordings",
		S3AccessKeyID:     "access-key-269",
		S3SecretAccessKey: secret,
		ClearS3Secret:     clearS3Secret,
		RetentionDays:     90,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal RecorderRequest: %v", err)
	}
	return string(body)
}
func TestHandleUpdateRecorderClearS3SecretConflictsWithNewValue(t *testing.T) {
	t.Parallel()
	fixture := seededSensitiveServer(t)
	rec := putRecorder(t, fixture.server, fixture.recorderID, recorderUpdateBody(t, "new-s3-secret", true))
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorEqual(t, rec, "clear_s3_secret: conflicts with non-empty s3_secret_access_key")
	recorder := fixture.server.config.Recorder(fixture.recorderID)
	if recorder == nil {
		t.Fatal("recorder not found")
	}
	if recorder.S3SecretAccessKey != fixture.s3Secret {
		t.Fatalf("recorder S3 secret = %q, want unchanged %q", recorder.S3SecretAccessKey, fixture.s3Secret)
	}
}
func TestNotificationTestEndpointSavedFallbacks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seed       func() *config.SettingsUpdate
		path       string
		body       string
		wantStatus int
		wantError  string
		contains   bool
		call       func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:       "email omitted secret falls back",
			seed:       seededGraphSettings,
			path:       "/api/notifications/test/email",
			body:       `{}`,
			wantStatus: http.StatusBadGateway,
			wantError:  "tenant ID must be a valid GUID",
			contains:   true,
			call:       (*Server).handleAPITestEmail,
		},
		{
			name:       "email explicit empty secret overrides",
			seed:       seededGraphSettings,
			path:       "/api/notifications/test/email",
			body:       `{"graph_client_secret":""}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Email not fully configured",
			call:       (*Server).handleAPITestEmail,
		},
		{
			name:       "zabbix omitted server falls back",
			seed:       seededZabbixSettings,
			path:       "/api/notifications/test/zabbix",
			body:       `{"zabbix_port":70000}`,
			wantStatus: http.StatusBadGateway,
			wantError:  "not fully configured",
			contains:   true,
			call:       (*Server).handleAPITestZabbix,
		},
		{
			name:       "zabbix explicit empty server overrides",
			seed:       seededZabbixSettings,
			path:       "/api/notifications/test/zabbix",
			body:       `{"zabbix_server":""}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Zabbix not fully configured",
			call:       (*Server).handleAPITestZabbix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := seededServer(t, tt.seed())
			rec := runJSONHandler(t, func(w http.ResponseWriter, r *http.Request) {
				tt.call(s, w, r)
			}, http.MethodPost, tt.path, tt.body)
			assertStatus(t, rec, tt.wantStatus)
			if tt.contains {
				assertErrorContains(t, rec, tt.wantError)
			} else {
				assertErrorEqual(t, rec, tt.wantError)
			}
		})
	}
}

// validBaselineSettings builds an update that preserves all currently valid settings.
func validBaselineSettings(cfg *config.Config) *config.SettingsUpdate {
	snap := cfg.Snapshot()
	return &config.SettingsUpdate{
		AudioInput:                  snap.AudioInput,
		SilenceThreshold:            snap.SilenceThreshold,
		SilenceDurationMs:           snap.SilenceDurationMs,
		SilenceRecoveryMs:           snap.SilenceRecoveryMs,
		PeakHoldMs:                  snap.PeakHoldMs,
		ChannelImbalanceThreshold:   snap.ChannelImbalanceThreshold,
		ChannelImbalanceDurationMs:  snap.ChannelImbalanceDurationMs,
		ChannelImbalanceRecoveryMs:  snap.ChannelImbalanceRecoveryMs,
		SilenceDumpEnabled:          snap.SilenceDumpEnabled,
		SilenceDumpRetentionDays:    snap.SilenceDumpRetentionDays,
		WebhookURL:                  snap.WebhookURL,
		WebhookEvents:               snap.WebhookEvents,
		EmailEvents:                 snap.EmailEvents,
		GraphTenantID:               snap.GraphTenantID,
		GraphClientID:               snap.GraphClientID,
		GraphClientSecret:           snap.GraphClientSecret,
		GraphFromAddress:            snap.GraphFromAddress,
		GraphRecipients:             snap.GraphRecipients,
		ZabbixEvents:                snap.ZabbixEvents.ToZabbixEventSubscriptions(),
		ZabbixServer:                snap.ZabbixServer,
		ZabbixPort:                  snap.ZabbixPort,
		ZabbixHost:                  snap.ZabbixHost,
		ZabbixSilenceKey:            snap.ZabbixSilenceKey,
		ZabbixUploadKey:             snap.ZabbixUploadKey,
		RecordingMaxDurationMinutes: snap.RecordingMaxDurationMinutes,
	}
}

// applyWithPreserve mirrors the settings API hidden-value preservation flow.
func applyWithPreserve(t *testing.T, cfg *config.Config, upd *config.SettingsUpdate) (config.Snapshot, error) {
	t.Helper()
	snap := cfg.Snapshot()
	preserveHiddenSettings(upd, &snap)
	if errs := upd.Validate(); len(errs) > 0 {
		return config.Snapshot{}, fmt.Errorf("validate: %s", strings.Join(errs, "; "))
	}
	if err := cfg.ApplySettings(upd); err != nil {
		return config.Snapshot{}, err
	}
	return cfg.Snapshot(), nil
}
func TestApplyWithPreserveHiddenValues(t *testing.T) {
	t.Parallel()
	const savedWebhookURL = "https://hooks.example.com/saved-token"
	tests := []struct {
		name   string
		seed   func() *config.SettingsUpdate
		mutate func(*config.SettingsUpdate)
		read   func(config.Snapshot) string
		want   string
	}{
		{
			name: "webhook keep",
			seed: func() *config.SettingsUpdate { return &config.SettingsUpdate{WebhookURL: savedWebhookURL} },
			mutate: func(upd *config.SettingsUpdate) {
				upd.WebhookURL = ""
				upd.ClearWebhookURL = false
			},
			read: func(snap config.Snapshot) string { return snap.WebhookURL },
			want: savedWebhookURL,
		},
		{
			name: "webhook replace",
			seed: func() *config.SettingsUpdate { return &config.SettingsUpdate{WebhookURL: savedWebhookURL} },
			mutate: func(upd *config.SettingsUpdate) {
				upd.WebhookURL = "https://hooks.example.com/new-token"
				upd.ClearWebhookURL = false
			},
			read: func(snap config.Snapshot) string { return snap.WebhookURL },
			want: "https://hooks.example.com/new-token",
		},
		{
			name: "webhook clear",
			seed: func() *config.SettingsUpdate { return &config.SettingsUpdate{WebhookURL: savedWebhookURL} },
			mutate: func(upd *config.SettingsUpdate) {
				upd.WebhookURL = ""
				upd.ClearWebhookURL = true
			},
			read: func(snap config.Snapshot) string { return snap.WebhookURL },
		},
		{
			name: "graph keep",
			seed: seededGraphSettings,
			mutate: func(upd *config.SettingsUpdate) {
				upd.GraphClientSecret = ""
				upd.ClearGraphClientSecret = false
			},
			read: func(snap config.Snapshot) string { return snap.GraphClientSecret },
			want: "saved-secret",
		},
		{
			name: "graph replace",
			seed: seededGraphSettings,
			mutate: func(upd *config.SettingsUpdate) {
				upd.GraphClientSecret = "new-secret"
				upd.ClearGraphClientSecret = false
			},
			read: func(snap config.Snapshot) string { return snap.GraphClientSecret },
			want: "new-secret",
		},
		{
			name: "graph clear",
			seed: seededGraphSettings,
			mutate: func(upd *config.SettingsUpdate) {
				upd.GraphClientSecret = ""
				upd.ClearGraphClientSecret = true
			},
			read: func(snap config.Snapshot) string { return snap.GraphClientSecret },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := seededServer(t, tt.seed())
			upd := validBaselineSettings(s.config)
			tt.mutate(upd)
			snap, err := applyWithPreserve(t, s.config, upd)
			if err != nil {
				t.Fatalf("applyWithPreserve() error = %v", err)
			}
			if got := tt.read(snap); got != tt.want {
				t.Fatalf("hidden value = %q, want %q", got, tt.want)
			}
		})
	}
}

func postSettingsBody(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.handleAPISettings(rec, req)
	return rec
}
func TestHandleAPISettingsClearHiddenValueConflictsWithNewValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		seed   func() *config.SettingsUpdate
		mutate func(*config.SettingsUpdate)
		want   string
	}{
		{
			name: "graph secret",
			seed: seededGraphSettings,
			mutate: func(upd *config.SettingsUpdate) {
				upd.GraphClientSecret = "new-secret"
				upd.ClearGraphClientSecret = true
			},
			want: "clear_graph_client_secret: conflicts with non-empty graph_client_secret",
		},
		{
			name: "webhook url",
			seed: func() *config.SettingsUpdate {
				return &config.SettingsUpdate{WebhookURL: "https://hooks.example.com/saved-token"}
			},
			mutate: func(upd *config.SettingsUpdate) {
				upd.WebhookURL = "https://hooks.example.com/new-token"
				upd.ClearWebhookURL = true
			},
			want: "clear_webhook_url: conflicts with non-empty webhook_url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := seededServer(t, tt.seed())
			upd := validBaselineSettings(s.config)
			tt.mutate(upd)
			body, err := json.Marshal(upd)
			if err != nil {
				t.Fatalf("marshal SettingsUpdate: %v", err)
			}
			rec := postSettingsBody(t, s, string(body))
			assertStatus(t, rec, http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body = %s, want to contain %q", rec.Body.String(), tt.want)
			}
		})
	}
}
