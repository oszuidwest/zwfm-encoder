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

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
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

// freshServer returns a Server with a default in-memory config for handler tests.
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

// TestHandleAPITestWebhookEmptyURLReturnsBadRequest pins the historical
// preflight behavior: an empty webhook URL is rejected at the handler with
// HTTP 400 and the user-visible message "No webhook URL configured".
// Non-empty URLs (even malformed) fall through to the runtime layer and
// surface as HTTP 502 - see TestHandleAPITestWebhookInvalidURLReachesRuntime.
func TestHandleAPITestWebhookEmptyURLReturnsBadRequest(t *testing.T) {
	t.Parallel()

	s := freshServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/webhook", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	s.handleAPITestWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); got != "No webhook URL configured" {
		t.Fatalf("error = %q, want %q", got, "No webhook URL configured")
	}
}

// TestHandleAPITestEmailMissingCredentialsReturnsBadRequest pins the
// historical preflight: missing any of tenant_id/client_id/client_secret
// is rejected at the handler with HTTP 400 and the generic message
// "Email not fully configured" (per-field reporting only happens at
// config-load and settings-save).
func TestHandleAPITestEmailMissingCredentialsReturnsBadRequest(t *testing.T) {
	t.Parallel()

	s := freshServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/email", bytes.NewBufferString(`{
		"graph_tenant_id": "tenant",
		"graph_client_id": "client"
	}`))
	rec := httptest.NewRecorder()

	s.handleAPITestEmail(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); got != "Email not fully configured" {
		t.Fatalf("error = %q, want %q", got, "Email not fully configured")
	}
}

// TestHandleAPITestZabbixOutOfRangePortReachesRuntime pins the historical
// status-code split: an incomplete Zabbix config is rejected by the handler
// preflight (400), but a complete config with an out-of-range port falls
// through to SendZabbixTest and surfaces as HTTP 502. The refactor preserves
// this split intentionally - port-range checks at the handler would have
// shifted port=70000 from 502 to 400 (a behavior change out of scope for #249).
func TestHandleAPITestZabbixOutOfRangePortReachesRuntime(t *testing.T) {
	t.Parallel()

	s := freshServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/zabbix", bytes.NewBufferString(`{
		"zabbix_server": "zabbix.example.com",
		"zabbix_port": 70000,
		"zabbix_host": "encoder-01",
		"zabbix_silence_key": "silence"
	}`))
	rec := httptest.NewRecorder()

	s.handleAPITestZabbix(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); !strings.Contains(got, "not fully configured") {
		t.Fatalf("error = %q, want substring %q", got, "not fully configured")
	}
}

// TestHandleTestS3MissingFieldReturnsBadRequest pins handler behavior: a
// missing S3 credential field is rejected with HTTP 400 and the field-
// specific message "<field> is required" (different format than
// Recorder.Validate's "<field>: is required for s3/both storage mode").
// First-error semantics: only the first missing field is reported.
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
			req := httptest.NewRequest(http.MethodPost, "/api/recorders/test-s3", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()

			s.handleTestS3(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if got := decodeError(t, rec.Body.Bytes()); got != tt.wantErr {
				t.Fatalf("error = %q, want %q", got, tt.wantErr)
			}
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
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/recorders/test-s3",
		bytes.NewBufferString(`{
			"recorder_id": "recorder-missing",
			"s3_bucket": "b",
			"s3_access_key_id": "k"
		}`),
	)
	rec := httptest.NewRecorder()

	s.handleTestS3(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); got != "Recorder not found" {
		t.Fatalf("error = %q, want %q", got, "Recorder not found")
	}
}

// seededServer returns a Server with saved config loaded from the given
// SettingsUpdate. Required range fields (silence threshold/durations/peak
// hold) get sensible defaults so callers only need to specify domain-
// specific values relevant to their test. Used by #243 regression tests
// to set up saved snapshots that handlers will read from.
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
	if err := cfg.ApplySettings(upd); err != nil {
		t.Fatalf("ApplySettings() error = %v", err)
	}
	return &Server{config: cfg}
}

// seededGraphSettings returns a saved Graph config with non-GUID tenant/
// client so any path through SendTestEmail's ValidateConfig fails
// deterministically with a strict-GUID error (before any network call).
// Config-load only checks email format, not GUID, so this seed loads cleanly.
func seededGraphSettings() *config.SettingsUpdate {
	return &config.SettingsUpdate{
		GraphTenantID:     "tenant-not-a-guid",
		GraphClientID:     "client-not-a-guid",
		GraphClientSecret: "saved-secret",
		GraphFromAddress:  "from@example.com",
		GraphRecipients:   "to@example.com",
	}
}

// seededZabbixSettings returns a complete saved Zabbix config that loads
// cleanly. Tests override individual fields via the request body to
// exercise the omitted-vs-explicit-empty distinction.
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
	fixture := sensitiveFixture{ //nolint:gosec // G101: intentional secret-shaped test fixture values verify redaction.
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

// TestHandleAPIConfigRedactsStoredSecrets pins #269 for the full frontend
// config snapshot. Sensitive stored values must never be serialized back to
// the browser; only presence booleans are exposed.
func TestHandleAPIConfigRedactsStoredSecrets(t *testing.T) {
	t.Parallel()

	fixture := seededSensitiveServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", http.NoBody)

	fixture.server.handleAPIConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
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

	var resp types.APIConfigResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response JSON = %q, error = %v", string(body), err)
	}
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

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			assertNotContains(t, rec.Body.Bytes(), fixture.streamPassword, `"password":"`)
			assertContains(t, rec.Body.Bytes(), `"has_password":true`)
		})
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

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			assertNotContains(t, rec.Body.Bytes(), fixture.s3Secret, `"s3_secret_access_key":"`)
			assertContains(t, rec.Body.Bytes(), `"has_s3_secret":true`)
		})
	}
}

func TestRedactionHelpersOmitSecretFields(t *testing.T) {
	t.Parallel()

	streamBody, err := json.Marshal(redactStream(&types.Stream{
		Password: "helper-stream-secret",
	}))
	if err != nil {
		t.Fatalf("marshal redacted stream: %v", err)
	}
	assertNotContains(t, streamBody, "helper-stream-secret", `"password":"`)
	assertContains(t, streamBody, `"has_password":true`)

	recorderBody, err := json.Marshal(redactRecorder(&types.Recorder{
		S3SecretAccessKey: "helper-recorder-secret",
	}))
	if err != nil {
		t.Fatalf("marshal redacted recorder: %v", err)
	}
	assertNotContains(t, recorderBody, "helper-recorder-secret", `"s3_secret_access_key":"`)
	assertContains(t, recorderBody, `"has_s3_secret":true`)
}

// TestHandleAPITestEmailOmittedSecretFallsBackToSaved (#243) pins that a
// request omitting graph_client_secret falls back to the saved secret. If
// the deref-based merge regressed (e.g. back to cmp.Or), the preflight
// would not see the saved secret and would return 400 "Email not fully
// configured". Instead we expect SendTestEmail's strict-GUID ValidateConfig
// to surface the saved (non-GUID) tenant as a 502.
func TestHandleAPITestEmailOmittedSecretFallsBackToSaved(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())

	// All fields omitted, so all fall back to saved snapshot.
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/email", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	s.handleAPITestEmail(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); !strings.Contains(got, "tenant ID must be a valid GUID") {
		t.Fatalf("error = %q, want strict-GUID validation error (proves preflight passed using saved secret)", got)
	}
}

// TestHandleAPITestEmailExplicitEmptySecretOverridesSaved (#243) pins that
// an explicit empty graph_client_secret overrides the saved value, causing
// preflight CredentialsIssues to reject with 400. This is the symmetric
// half of the omitted-fallback test and proves the deref distinguishes
// nil from *"".
func TestHandleAPITestEmailExplicitEmptySecretOverridesSaved(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/email", bytes.NewBufferString(`{"graph_client_secret":""}`))
	rec := httptest.NewRecorder()

	s.handleAPITestEmail(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); got != "Email not fully configured" {
		t.Fatalf("error = %q, want %q", got, "Email not fully configured")
	}
}

// TestHandleAPITestZabbixOmittedServerFallsBackToSaved (#243) pins that a
// request omitting zabbix_server falls back to the saved server. The
// request overrides zabbix_port to 70000 so SendZabbixTest's
// ValidateZabbixTarget rejects deterministically before any network call;
// reaching that runtime path requires preflight to have seen the saved
// server (otherwise we'd get 400 "Zabbix not fully configured").
func TestHandleAPITestZabbixOmittedServerFallsBackToSaved(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededZabbixSettings())

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/zabbix", bytes.NewBufferString(`{"zabbix_port":70000}`))
	rec := httptest.NewRecorder()

	s.handleAPITestZabbix(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); !strings.Contains(got, "not fully configured") {
		t.Fatalf("error = %q, want runtime not-fully-configured (proves omitted server fell back)", got)
	}
}

// TestHandleAPITestZabbixExplicitEmptyServerOverridesSaved (#243) pins
// that an explicit empty zabbix_server overrides the saved value, causing
// handler preflight ValidateZabbixConfigured to reject with 400.
func TestHandleAPITestZabbixExplicitEmptyServerOverridesSaved(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededZabbixSettings())

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/zabbix", bytes.NewBufferString(`{"zabbix_server":""}`))
	rec := httptest.NewRecorder()

	s.handleAPITestZabbix(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()); got != "Zabbix not fully configured" {
		t.Fatalf("error = %q, want %q", got, "Zabbix not fully configured")
	}
}

// validBaselineSettings returns a SettingsUpdate built from cfg.Snapshot()
// with all required fields filled to current valid values. Callers override
// only the fields under test (e.g. GraphClientSecret + ClearGraphClientSecret).
// Without this, tests would fail with unrelated validator errors
// ("silence_threshold must be between -60 and -1 dB", etc.).
func validBaselineSettings(cfg *config.Config) *config.SettingsUpdate {
	snap := cfg.Snapshot()
	return &config.SettingsUpdate{
		AudioInput:                  snap.AudioInput,
		SilenceThreshold:            snap.SilenceThreshold,
		SilenceDurationMs:           snap.SilenceDurationMs,
		SilenceRecoveryMs:           snap.SilenceRecoveryMs,
		PeakHoldMs:                  snap.PeakHoldMs,
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

// applyWithPreserve mirrors handleAPISettings preprocessing: preserve hidden
// values, validate, and apply. Handler success paths need a real encoder, so
// these tests exercise the same settings pipeline directly.
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

// #269 - Webhook URL keep/replace/clear pipeline tests.

func TestApplyWithPreserve_WebhookURL_KeepWhenEmpty(t *testing.T) {
	t.Parallel()

	s := seededServer(t, &config.SettingsUpdate{WebhookURL: "https://hooks.example.com/saved-token"})
	upd := validBaselineSettings(s.config)
	upd.WebhookURL = ""
	upd.ClearWebhookURL = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WebhookURL != "https://hooks.example.com/saved-token" {
		t.Fatalf("WebhookURL = %q, want saved URL (keep path)", snap.WebhookURL)
	}
}

func TestApplyWithPreserve_WebhookURL_ReplaceWhenSet(t *testing.T) {
	t.Parallel()

	s := seededServer(t, &config.SettingsUpdate{WebhookURL: "https://hooks.example.com/saved-token"})
	upd := validBaselineSettings(s.config)
	upd.WebhookURL = "https://hooks.example.com/new-token"
	upd.ClearWebhookURL = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WebhookURL != "https://hooks.example.com/new-token" {
		t.Fatalf("WebhookURL = %q, want new URL (replace path)", snap.WebhookURL)
	}
}

func TestApplyWithPreserve_WebhookURL_ClearWhenFlagSet(t *testing.T) {
	t.Parallel()

	s := seededServer(t, &config.SettingsUpdate{WebhookURL: "https://hooks.example.com/saved-token"})
	upd := validBaselineSettings(s.config)
	upd.WebhookURL = ""
	upd.ClearWebhookURL = true

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WebhookURL != "" {
		t.Fatalf("WebhookURL = %q, want \"\" (clear path)", snap.WebhookURL)
	}
}

// #247 - Graph secret keep/replace/clear pipeline tests.

// TestApplyWithPreserve_GraphSecret_KeepWhenEmpty pins that an empty submitted
// secret with ClearGraphClientSecret=false falls back to the saved secret via
// the preserve helper.
func TestApplyWithPreserve_GraphSecret_KeepWhenEmpty(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())
	upd := validBaselineSettings(s.config)
	upd.GraphClientSecret = ""
	upd.ClearGraphClientSecret = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.GraphClientSecret != "saved-secret" {
		t.Fatalf("GraphClientSecret = %q, want %q (keep path)", snap.GraphClientSecret, "saved-secret")
	}
}

// TestApplyWithPreserve_GraphSecret_ReplaceWhenSet pins that a non-empty
// submitted secret replaces the saved secret regardless of preserve.
func TestApplyWithPreserve_GraphSecret_ReplaceWhenSet(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())
	upd := validBaselineSettings(s.config)
	upd.GraphClientSecret = "new-secret"
	upd.ClearGraphClientSecret = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.GraphClientSecret != "new-secret" {
		t.Fatalf("GraphClientSecret = %q, want %q (replace path)", snap.GraphClientSecret, "new-secret")
	}
}

// TestApplyWithPreserve_GraphSecret_ClearWhenFlagSet pins that an empty
// submitted secret with ClearGraphClientSecret=true removes the saved secret.
// This is the supported "remove saved secret" path.
func TestApplyWithPreserve_GraphSecret_ClearWhenFlagSet(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())
	upd := validBaselineSettings(s.config)
	upd.GraphClientSecret = ""
	upd.ClearGraphClientSecret = true

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.GraphClientSecret != "" {
		t.Fatalf("GraphClientSecret = %q, want \"\" (clear path)", snap.GraphClientSecret)
	}
}

// #247 - Handler validation paths.

// postSettingsBody sends body to handleAPISettings and returns the response
// recorder so callers can assert status and error body. Handler tests only cover
// 400 paths, which return before encoder calls; success paths are covered by
// applyWithPreserve.
func postSettingsBody(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.handleAPISettings(rec, req)
	return rec
}

// TestHandleAPISettingsClearGraphSecretConflictsWithNewValue pins #247:
// submitting both clear_graph_client_secret=true and a non-empty
// graph_client_secret returns 400 with the conflict error. Proves the
// preserve helper does not silently blank the submitted value on clear=true
// (which would let clear win without telling the user).
func TestHandleAPISettingsClearGraphSecretConflictsWithNewValue(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())
	upd := validBaselineSettings(s.config)
	upd.GraphClientSecret = "new-secret"
	upd.ClearGraphClientSecret = true

	body, err := json.Marshal(upd)
	if err != nil {
		t.Fatalf("marshal SettingsUpdate: %v", err)
	}
	rec := postSettingsBody(t, s, string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clear_graph_client_secret: conflicts with non-empty graph_client_secret") {
		t.Fatalf("body = %s, want to contain conflict error", rec.Body.String())
	}
}

func TestHandleAPISettingsClearWebhookURLConflictsWithNewValue(t *testing.T) {
	t.Parallel()

	s := seededServer(t, &config.SettingsUpdate{WebhookURL: "https://hooks.example.com/saved-token"})
	upd := validBaselineSettings(s.config)
	upd.WebhookURL = "https://hooks.example.com/new-token"
	upd.ClearWebhookURL = true

	body, err := json.Marshal(upd)
	if err != nil {
		t.Fatalf("marshal SettingsUpdate: %v", err)
	}
	rec := postSettingsBody(t, s, string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clear_webhook_url: conflicts with non-empty webhook_url") {
		t.Fatalf("body = %s, want to contain conflict error", rec.Body.String())
	}
}
