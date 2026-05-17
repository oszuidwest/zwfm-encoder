package main

import (
	"bytes"
	"encoding/json"
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

// TestPreserveWhatsAppAccessTokenAllowsDisable verifies that submitting an
// all-empty WhatsApp settings save does NOT silently restore the saved token.
// Without this guard, a saved token combined with an empty-form save would trip
// validateWhatsAppConfigFields' all-or-nothing check and 400 the user out of
// disabling WhatsApp. Companion to the Graph asymmetry comment on the helper.
func TestPreserveWhatsAppAccessTokenAllowsDisable(t *testing.T) {
	t.Parallel()

	cfg := config.Snapshot{WhatsAppAccessToken: "saved-token"}
	req := config.SettingsUpdate{} // every WhatsApp field empty

	preserveWhatsAppAccessToken(&req, &cfg)

	if req.WhatsAppAccessToken != "" {
		t.Fatalf("WhatsAppAccessToken = %q, want \"\" (all visible fields empty must allow disable)",
			req.WhatsAppAccessToken)
	}
}

// TestPreserveWhatsAppAccessTokenInheritsSavedTokenWhenVisibleConfigRemains
// verifies the inverse: when the user keeps WhatsApp enabled but leaves the token
// blank in the form (so the UI can omit it), the saved token is restored.
func TestPreserveWhatsAppAccessTokenInheritsSavedTokenWhenVisibleConfigRemains(t *testing.T) {
	t.Parallel()

	cfg := config.Snapshot{WhatsAppAccessToken: "saved-token"}
	req := config.SettingsUpdate{
		WhatsAppPhoneNumberID: "12345",
		WhatsAppRecipients:    "+31612345678",
		// AccessToken left empty in the form
	}

	preserveWhatsAppAccessToken(&req, &cfg)

	if req.WhatsAppAccessToken != "saved-token" {
		t.Fatalf("WhatsAppAccessToken = %q, want %q (visible config remains, preserve saved token)",
			req.WhatsAppAccessToken, "saved-token")
	}
}

// TestPreserveWhatsAppAccessTokenTreatsWhitespaceOnlyFieldsAsEmpty mirrors the
// UI's TrimSpace contract: a form that contains only whitespace counts as empty
// and the disable path must still be reachable.
func TestPreserveWhatsAppAccessTokenTreatsWhitespaceOnlyFieldsAsEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.Snapshot{WhatsAppAccessToken: "saved-token"}
	req := config.SettingsUpdate{
		WhatsAppPhoneNumberID: "  ",
		WhatsAppRecipients:    "\t",
	}

	preserveWhatsAppAccessToken(&req, &cfg)

	if req.WhatsAppAccessToken != "" {
		t.Fatalf("WhatsAppAccessToken = %q, want \"\" (whitespace-only fields must be treated as empty)",
			req.WhatsAppAccessToken)
	}
}

// TestPreserveWhatsAppAccessTokenIgnoresStaleLanguage verifies that template
// language alone is not a "visible config" signal. The UI pre-fills language
// from the saved config, so a user who clears every other WhatsApp field can
// still have a stale "en_US" in the request; that must not silently restore
// the saved token and trip the all-or-nothing partial-config check.
func TestPreserveWhatsAppAccessTokenIgnoresStaleLanguage(t *testing.T) {
	t.Parallel()

	cfg := config.Snapshot{WhatsAppAccessToken: "saved-token"}
	req := config.SettingsUpdate{
		WhatsAppTemplateLanguage: "en_US",
	}

	preserveWhatsAppAccessToken(&req, &cfg)

	if req.WhatsAppAccessToken != "" {
		t.Fatalf("WhatsAppAccessToken = %q, want \"\" (language alone must not preserve token)",
			req.WhatsAppAccessToken)
	}
}

// TestPrepareWhatsAppSettingsRequestAllowsDisableWithStaleLanguage covers the
// UI-realistic disable path: the user clears phone, recipients, template name,
// and the token input, but the form pre-fill leaves a stale "en_US" template
// language in the request. The prepare helper must produce an all-empty WhatsApp
// config so the SettingsUpdate validator accepts the disable. Guards against
// both the order bug (preserve before normalize) and any regression that lets
// language alone count as visible config.
func TestPrepareWhatsAppSettingsRequestAllowsDisableWithStaleLanguage(t *testing.T) {
	t.Parallel()

	cfg := config.Snapshot{
		WhatsAppAccessToken:      "saved-token",
		WhatsAppTemplateLanguage: "en_US",
	}
	req := config.SettingsUpdate{
		WhatsAppTemplateLanguage: "en_US", // stale form pre-fill, every other field cleared
	}

	prepareWhatsAppSettingsRequest(&req, &cfg)

	if req.WhatsAppAccessToken != "" {
		t.Fatalf("WhatsAppAccessToken = %q, want \"\" (stale language must not block disable)",
			req.WhatsAppAccessToken)
	}
	if req.WhatsAppTemplateLanguage != "" {
		t.Fatalf("WhatsAppTemplateLanguage = %q, want \"\" (normalize must drop language when name is empty)",
			req.WhatsAppTemplateLanguage)
	}

	// Belt-and-suspenders: the resulting SettingsUpdate must pass partial-config validation.
	// validateWhatsAppConfigFields treats any non-empty WhatsApp field as a sign of
	// partial configuration and then demands the rest; the disable path requires *all* empty.
	for _, e := range req.Validate() {
		if strings.Contains(e, "whatsapp_") {
			t.Fatalf("Validate() returned WhatsApp error after disable prep: %q", e)
		}
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

// TestHandleAPITestWebhookEmptyURLReturnsBadRequest pins the historical
// preflight behavior: an empty webhook URL is rejected at the handler with
// HTTP 400 and the user-visible message "No webhook URL configured".
// Non-empty URLs (even malformed) fall through to the runtime layer and
// surface as HTTP 502 — see TestHandleAPITestWebhookInvalidURLReachesRuntime.
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
// this split intentionally — port-range checks at the handler would have
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
