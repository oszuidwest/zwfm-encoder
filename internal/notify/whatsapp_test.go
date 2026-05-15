package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestSendWhatsAppTestSendsTextToRecipients(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	var mu sync.Mutex
	var got []whatsappTextRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/12345/messages" {
			t.Errorf("path = %s, want /12345/messages", r.URL.Path)
		}
		if gotAuth := r.Header.Get("Authorization"); gotAuth != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", gotAuth)
		}

		var payload whatsappTextRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
			return
		}

		mu.Lock()
		got = append(got, payload)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.test"}]}`))
	}))
	defer server.Close()

	whatsappAPIBaseURL = server.URL
	whatsappClient = server.Client()

	cfg := &WhatsAppConfig{
		PhoneNumberID: "12345",
		AccessToken:   "test-token",
		Recipients:    "+316 123-45678, 31687654321",
	}

	if err := SendWhatsAppTest(cfg, "Test Station"); err != nil {
		t.Fatalf("SendWhatsAppTest() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("request count = %d, want 2", len(got))
	}
	if got[0].To != "31612345678" {
		t.Fatalf("first recipient = %q, want 31612345678", got[0].To)
	}
	if got[1].To != "31687654321" {
		t.Fatalf("second recipient = %q, want 31687654321", got[1].To)
	}
	if got[0].MessagingProduct != "whatsapp" || got[0].Type != "text" {
		t.Fatalf("payload = %+v, want whatsapp text payload", got[0])
	}
	if !strings.Contains(got[0].Text.Body, "Test WhatsApp notification") {
		t.Fatalf("body = %q, want test notification text", got[0].Text.Body)
	}
}

func TestSendWhatsAppTestReturnsAPIError(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token","code":190}}`))
	}))
	defer server.Close()

	whatsappAPIBaseURL = server.URL
	whatsappClient = server.Client()

	cfg := &WhatsAppConfig{
		PhoneNumberID: "12345",
		AccessToken:   "bad-token",
		Recipients:    "+31612345678",
	}

	err := SendWhatsAppTest(cfg, "Test Station")
	if err == nil {
		t.Fatal("SendWhatsAppTest() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "Invalid OAuth access token") {
		t.Fatalf("SendWhatsAppTest() error = %q, want API error details", err.Error())
	}
}

func TestSendWhatsAppTestUsesTemplateWhenConfigured(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	var got whatsappTemplateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.test"}]}`))
	}))
	defer server.Close()

	whatsappAPIBaseURL = server.URL
	whatsappClient = server.Client()

	cfg := &WhatsAppConfig{
		PhoneNumberID:    "12345",
		AccessToken:      "test-token",
		Recipients:       "+31612345678",
		TemplateName:     "encoder_alert",
		TemplateLanguage: "nl",
	}

	if err := SendWhatsAppTest(cfg, "Test Station"); err != nil {
		t.Fatalf("SendWhatsAppTest() error = %v", err)
	}

	if got.Type != "template" {
		t.Fatalf("Type = %q, want template", got.Type)
	}
	if got.Template.Name != "encoder_alert" {
		t.Fatalf("Template.Name = %q, want encoder_alert", got.Template.Name)
	}
	if got.Template.Language.Code != "nl" {
		t.Fatalf("Template.Language.Code = %q, want nl", got.Template.Language.Code)
	}
	if len(got.Template.Components) != 1 || len(got.Template.Components[0].Parameters) != 1 {
		t.Fatalf("Template.Components = %+v, want one body parameter", got.Template.Components)
	}
	if !strings.Contains(got.Template.Components[0].Parameters[0].Text, "Test WhatsApp notification") {
		t.Fatalf("template parameter text = %q, want test notification text", got.Template.Components[0].Parameters[0].Text)
	}
}

func TestWhatsAppChannelSubscriptions(t *testing.T) {
	t.Parallel()

	ch := &WhatsAppChannel{}
	cfg := &config.Snapshot{
		WhatsAppPhoneNumberID: "12345",
		WhatsAppAccessToken:   "token",
		WhatsAppRecipients:    "+31612345678",
		WhatsAppEvents: types.EventSubscriptions{
			SilenceStart: true,
			SilenceEnd:   false,
			AudioDump:    true,
		},
	}

	if !ch.IsConfiguredForSilence(cfg) {
		t.Fatal("IsConfiguredForSilence() = false, want true")
	}
	if !ch.SubscribesSilenceStart(cfg) {
		t.Fatal("SubscribesSilenceStart() = false, want true")
	}
	if ch.SubscribesSilenceEnd(cfg) {
		t.Fatal("SubscribesSilenceEnd() = true, want false")
	}
	if !ch.SubscribesAudioDump(cfg) {
		t.Fatal("SubscribesAudioDump() = false, want true")
	}
}
