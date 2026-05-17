package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
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

func TestHandleAPITestWhatsAppValidationErrorReturnsBadRequest(t *testing.T) {
	t.Parallel()

	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	s := &Server{config: cfg}
	body := bytes.NewBufferString(`{
		"whatsapp_phone_number_id": "12345",
		"whatsapp_access_token": "token",
		"whatsapp_recipients": "bad-address"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test/whatsapp", body)
	rec := httptest.NewRecorder()

	s.handleAPITestWhatsApp(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response JSON = %q, error = %v", rec.Body.String(), err)
	}
	if !strings.Contains(payload["error"], "recipient") {
		t.Fatalf("error = %q, want recipient validation message", payload["error"])
	}
}

// parseWhatsAppTestRequest unmarshals a WhatsApp test request JSON body, mirroring
// what handleAPITestWhatsApp does before calling mergeWhatsAppTestConfig.
func parseWhatsAppTestRequest(t *testing.T, body string) NotificationTestRequest {
	t.Helper()
	var req NotificationTestRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	return req
}

// savedWhatsAppSnapshot mirrors a saved config that has a WhatsApp template configured.
// The merge tests verify whether request fields fall back to or override these values.
var savedWhatsAppSnapshot = config.Snapshot{
	WhatsAppPhoneNumberID:    "12345",
	WhatsAppAccessToken:      "saved-token",
	WhatsAppRecipients:       "+31612345678",
	WhatsAppTemplateName:     "saved_template",
	WhatsAppTemplateLanguage: "en_US",
}

// TestMergeWhatsAppTestConfigOmittedTemplateFallsBackToSaved verifies that a request
// without a `whatsapp_template_name` field inherits the saved template (#243 regression).
// This is the path that drove pointer fields + deref over cmp.Or: omitted JSON keys
// must keep using the saved value.
func TestMergeWhatsAppTestConfigOmittedTemplateFallsBackToSaved(t *testing.T) {
	t.Parallel()

	req := parseWhatsAppTestRequest(t, `{}`)
	got := mergeWhatsAppTestConfig(&req, &savedWhatsAppSnapshot)

	if got.TemplateName != "saved_template" {
		t.Fatalf("TemplateName = %q, want %q (omitted field must fall back to saved value)",
			got.TemplateName, "saved_template")
	}
	if got.TemplateLanguage != "en_US" {
		t.Fatalf("TemplateLanguage = %q, want %q (omitted field must fall back to saved value)",
			got.TemplateLanguage, "en_US")
	}
}

// TestMergeWhatsAppTestConfigExplicitEmptyTemplateClearsSaved verifies that an
// explicit `"whatsapp_template_name": ""` overrides the saved value rather than
// falling back to it (#243 regression: the exact bug delta that would fail under
// the previous cmp.Or-based merge). Sending an empty template name is how the UI
// signals "send as free text on this one test", which must not be silently rewritten
// to the saved template.
func TestMergeWhatsAppTestConfigExplicitEmptyTemplateClearsSaved(t *testing.T) {
	t.Parallel()

	req := parseWhatsAppTestRequest(t, `{"whatsapp_template_name": "", "whatsapp_template_language": ""}`)
	got := mergeWhatsAppTestConfig(&req, &savedWhatsAppSnapshot)

	if got.TemplateName != "" {
		t.Fatalf("TemplateName = %q, want \"\" (explicit empty must override saved value)", got.TemplateName)
	}
	if got.TemplateLanguage != "" {
		t.Fatalf("TemplateLanguage = %q, want \"\" (explicit empty must override saved value)", got.TemplateLanguage)
	}
	// Sanity: untouched fields still fall back, so the merge is partial-override, not all-or-nothing.
	if got.PhoneNumberID != "12345" || got.AccessToken != "saved-token" || got.Recipients != "+31612345678" {
		t.Fatalf("unrelated fields lost saved values: %+v", got)
	}
}

// TestMergeWhatsAppTestConfigClearedTemplateNameDropsSavedLanguage verifies that
// clearing only the template name (while leaving template language at the saved
// value) does not produce a "language without name" config; that combination
// would fail downstream validation and surface as a confusing 400. The merge must
// drop the language so free-text mode is reachable by clearing just the name.
func TestMergeWhatsAppTestConfigClearedTemplateNameDropsSavedLanguage(t *testing.T) {
	t.Parallel()

	req := parseWhatsAppTestRequest(t, `{"whatsapp_template_name": ""}`)
	got := mergeWhatsAppTestConfig(&req, &savedWhatsAppSnapshot)

	if got.TemplateName != "" {
		t.Fatalf("TemplateName = %q, want \"\"", got.TemplateName)
	}
	if got.TemplateLanguage != "" {
		t.Fatalf("TemplateLanguage = %q, want \"\" (cleared name must also drop saved language)",
			got.TemplateLanguage)
	}
}

// Old WhatsApp implicit-disable preserve tests removed in #247: the
// "visible-field heuristic" is replaced by an explicit
// ClearWhatsAppAccessToken flag. Coverage for the new pipeline lives in
// TestApplyWithPreserve_WhatsAppToken_* and the handler-level conflict
// tests below. The stale-template-language path is re-covered by
// TestApplyWithPreserve_WhatsAppClearedTemplateNameDropsStaleLanguage.

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
// shifted port=70000 from 502 → 400 (a behavior change out of scope for #249).
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

// TestHandleAPITestEmailOmittedSecretFallsBackToSaved (#243) pins that a
// request omitting graph_client_secret falls back to the saved secret. If
// the deref-based merge regressed (e.g. back to cmp.Or), the preflight
// would NOT see the saved secret and would return 400 "Email not fully
// configured". Instead we expect SendTestEmail's strict-GUID ValidateConfig
// to surface the saved (non-GUID) tenant as a 502.
func TestHandleAPITestEmailOmittedSecretFallsBackToSaved(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededGraphSettings())

	// All fields omitted → all fall back to saved snapshot.
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

// seededWhatsAppCompleteSettings returns a complete saved WhatsApp config
// that loads cleanly and exercises all four visible fields plus a template
// language. Used by #247 keep/replace/clear pipeline tests where token
// preservation only makes sense with surrounding valid config (otherwise
// the all-or-nothing validator masks the behavior under test).
func seededWhatsAppCompleteSettings() *config.SettingsUpdate {
	return &config.SettingsUpdate{
		WhatsAppPhoneNumberID:    "12345",
		WhatsAppAccessToken:      "saved-token",
		WhatsAppRecipients:       "+31612345678",
		WhatsAppTemplateName:     "saved_template",
		WhatsAppTemplateLanguage: "en_US",
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
		WhatsAppPhoneNumberID:       snap.WhatsAppPhoneNumberID,
		WhatsAppAccessToken:         snap.WhatsAppAccessToken,
		WhatsAppRecipients:          snap.WhatsAppRecipients,
		WhatsAppTemplateName:        snap.WhatsAppTemplateName,
		WhatsAppTemplateLanguage:    snap.WhatsAppTemplateLanguage,
		WhatsAppEvents:              snap.WhatsAppEvents,
		ZabbixEvents:                snap.ZabbixEvents.ToZabbixEventSubscriptions(),
		ZabbixServer:                snap.ZabbixServer,
		ZabbixPort:                  snap.ZabbixPort,
		ZabbixHost:                  snap.ZabbixHost,
		ZabbixSilenceKey:            snap.ZabbixSilenceKey,
		ZabbixUploadKey:             snap.ZabbixUploadKey,
		RecordingMaxDurationMinutes: snap.RecordingMaxDurationMinutes,
	}
}

// applyWithPreserve mirrors handleAPISettings preprocess EXACTLY: preserve
// Graph secret, run the full WhatsApp prepare pipeline (normalize template
// language + preserve access token), validate, apply. Used by #247
// keep/replace/clear pipeline tests so we exercise the same logic the
// handler runs without needing a real encoder for 204 paths.
//
// Calling preserveWhatsAppAccessToken directly here would skip the
// template-language normalization and leave a coverage gap for the
// "clear name + stale language" path that prepareWhatsAppSettingsRequest
// exists to handle.
func applyWithPreserve(t *testing.T, cfg *config.Config, upd *config.SettingsUpdate) (config.Snapshot, error) {
	t.Helper()
	snap := cfg.Snapshot()
	preserveGraphClientSecret(upd, &snap)
	prepareWhatsAppSettingsRequest(upd, &snap)
	if errs := upd.Validate(); len(errs) > 0 {
		return config.Snapshot{}, fmt.Errorf("validate: %s", strings.Join(errs, "; "))
	}
	if err := cfg.ApplySettings(upd); err != nil {
		return config.Snapshot{}, err
	}
	return cfg.Snapshot(), nil
}

// #247 - Graph secret keep/replace/clear (Niveau 2 pipeline tests).

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
// This is the supported "remove saved secret" path that did not exist before
// #247.
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

// #247 - WhatsApp access token keep/replace/clear (Niveau 2 pipeline tests).

// TestApplyWithPreserve_WhatsAppToken_KeepWhenEmpty pins that an empty
// submitted token with visible WhatsApp fields still set falls back to the
// saved token via preserve. Replaces the deprecated implicit-disable path's
// "keep when visible config remains" test.
func TestApplyWithPreserve_WhatsAppToken_KeepWhenEmpty(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	upd.WhatsAppAccessToken = ""
	upd.ClearWhatsAppAccessToken = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WhatsAppAccessToken != "saved-token" {
		t.Fatalf("WhatsAppAccessToken = %q, want %q (keep path)", snap.WhatsAppAccessToken, "saved-token")
	}
	if snap.WhatsAppPhoneNumberID != "12345" {
		t.Fatalf("WhatsAppPhoneNumberID = %q, want %q (visible fields unchanged)", snap.WhatsAppPhoneNumberID, "12345")
	}
}

// TestApplyWithPreserve_WhatsAppToken_ReplaceWhenSet pins that a non-empty
// submitted token replaces the saved token.
func TestApplyWithPreserve_WhatsAppToken_ReplaceWhenSet(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	upd.WhatsAppAccessToken = "new-token"
	upd.ClearWhatsAppAccessToken = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WhatsAppAccessToken != "new-token" {
		t.Fatalf("WhatsAppAccessToken = %q, want %q (replace path)", snap.WhatsAppAccessToken, "new-token")
	}
}

// TestApplyWithPreserve_WhatsAppToken_ClearWhenFlagSetAndAllVisibleCleared
// pins the explicit-disable path that replaces the deprecated implicit
// disable: clearToken=true plus all visible WhatsApp fields cleared yields
// a fully disabled channel. Matches the UI's Disable WhatsApp button which
// clears all five fields together.
func TestApplyWithPreserve_WhatsAppToken_ClearWhenFlagSetAndAllVisibleCleared(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	upd.WhatsAppPhoneNumberID = ""
	upd.WhatsAppAccessToken = ""
	upd.WhatsAppRecipients = ""
	upd.WhatsAppTemplateName = ""
	upd.WhatsAppTemplateLanguage = ""
	upd.ClearWhatsAppAccessToken = true

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WhatsAppAccessToken != "" {
		t.Fatalf("WhatsAppAccessToken = %q, want \"\"", snap.WhatsAppAccessToken)
	}
	if snap.WhatsAppPhoneNumberID != "" || snap.WhatsAppRecipients != "" ||
		snap.WhatsAppTemplateName != "" || snap.WhatsAppTemplateLanguage != "" {
		t.Fatalf("WhatsApp visible fields not all empty: phone=%q recipients=%q template=%q language=%q",
			snap.WhatsAppPhoneNumberID, snap.WhatsAppRecipients,
			snap.WhatsAppTemplateName, snap.WhatsAppTemplateLanguage)
	}
}

// TestApplyWithPreserve_WhatsAppClearedTemplateNameDropsStaleLanguage covers
// the stale-template-language scenario that prepareWhatsAppSettingsRequest
// still exists to handle after #247. UI used to pre-fill template language
// from saved config; a user clearing only template_name in the form would
// leave a stale "en_US" in the request. The normalize step in
// prepareWhatsAppSettingsRequest must drop language so the all-or-nothing
// validator doesn't reject "language without name".
func TestApplyWithPreserve_WhatsAppClearedTemplateNameDropsStaleLanguage(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	// Stale form state: user cleared template_name but language pre-fill remains.
	upd.WhatsAppTemplateName = ""
	upd.WhatsAppTemplateLanguage = "en_US"
	// Token left empty in form - preserve must restore saved token.
	upd.WhatsAppAccessToken = ""
	upd.ClearWhatsAppAccessToken = false

	snap, err := applyWithPreserve(t, s.config, upd)
	if err != nil {
		t.Fatalf("applyWithPreserve() error = %v", err)
	}
	if snap.WhatsAppTemplateName != "" {
		t.Fatalf("WhatsAppTemplateName = %q, want \"\"", snap.WhatsAppTemplateName)
	}
	if snap.WhatsAppTemplateLanguage != "" {
		t.Fatalf("WhatsAppTemplateLanguage = %q, want \"\" (normalize must drop stale language)", snap.WhatsAppTemplateLanguage)
	}
	if snap.WhatsAppAccessToken != "saved-token" {
		t.Fatalf("WhatsAppAccessToken = %q, want %q (saved token must be preserved)", snap.WhatsAppAccessToken, "saved-token")
	}
	if snap.WhatsAppPhoneNumberID != "12345" {
		t.Fatalf("WhatsAppPhoneNumberID = %q, want %q (other visible fields unchanged)", snap.WhatsAppPhoneNumberID, "12345")
	}
}

// #247 - Handler 400 paths (Niveau 3).

// postSettingsBody marshals upd to JSON and runs handleAPISettings against
// the given server. Returns the response recorder so callers can assert
// status + error body. Handler tests only - 400 paths return before the
// encoder calls, so `&Server{config: cfg}` is safe; 204 paths need an
// encoder helper which is intentionally out of scope (see Niveau 2).
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
// preserve helper does NOT silently blank the submitted value on clear=true
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

// TestHandleAPISettingsClearWhatsAppTokenConflictsWithNewValue mirrors the
// Graph conflict test for WhatsApp.
func TestHandleAPISettingsClearWhatsAppTokenConflictsWithNewValue(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	upd.WhatsAppAccessToken = "new-token"
	upd.ClearWhatsAppAccessToken = true

	body, err := json.Marshal(upd)
	if err != nil {
		t.Fatalf("marshal SettingsUpdate: %v", err)
	}
	rec := postSettingsBody(t, s, string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clear_whatsapp_access_token: conflicts with non-empty whatsapp_access_token") {
		t.Fatalf("body = %s, want to contain conflict error", rec.Body.String())
	}
}

// TestHandleAPISettingsWhatsAppImplicitDisableNowFails pins the #247
// deprecation: emptying all visible WhatsApp fields without explicit
// clear_whatsapp_access_token=true used to disable the channel, now returns
// 400 from the all-or-nothing validator. The body MUST send
// whatsapp_access_token="" explicitly so the test exercises the deprecated
// implicit-disable path (token empty in request, saved token gets preserved
// back in via preserveWhatsAppAccessToken, validator rejects visible-empty
// with token preserved).
func TestHandleAPISettingsWhatsAppImplicitDisableNowFails(t *testing.T) {
	t.Parallel()

	s := seededServer(t, seededWhatsAppCompleteSettings())
	upd := validBaselineSettings(s.config)
	upd.WhatsAppPhoneNumberID = ""
	upd.WhatsAppAccessToken = "" // explicit empty - not omitted; senior review note
	upd.WhatsAppRecipients = ""
	upd.WhatsAppTemplateName = ""
	upd.WhatsAppTemplateLanguage = ""
	// No ClearWhatsAppAccessToken - this is the deprecated implicit-disable path.

	body, err := json.Marshal(upd)
	if err != nil {
		t.Fatalf("marshal SettingsUpdate: %v", err)
	}
	rec := postSettingsBody(t, s, string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "whatsapp_phone_number_id: is required when WhatsApp is configured") {
		t.Fatalf("body = %s, want to contain all-or-nothing validator error", rec.Body.String())
	}
}
