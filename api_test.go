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

func TestPreserveWhatsAppAccessTokenOnlyWhenVisibleFieldsRemain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  config.SettingsUpdate
		want string
	}{
		{
			name: "clear all whatsapp fields clears saved token",
			req:  config.SettingsUpdate{},
			want: "",
		},
		{
			name: "phone number keeps saved token",
			req: config.SettingsUpdate{
				WhatsAppPhoneNumberID: "12345",
			},
			want: "stored-token",
		},
		{
			name: "explicit token wins",
			req: config.SettingsUpdate{
				WhatsAppPhoneNumberID: "12345",
				WhatsAppAccessToken:   "new-token",
			},
			want: "new-token",
		},
	}

	cfg := &config.Snapshot{WhatsAppAccessToken: "stored-token"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := tt.req
			preserveWhatsAppAccessToken(&req, cfg)
			if got := req.WhatsAppAccessToken; got != tt.want {
				t.Fatalf("WhatsAppAccessToken = %q, want %q", got, tt.want)
			}
		})
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
