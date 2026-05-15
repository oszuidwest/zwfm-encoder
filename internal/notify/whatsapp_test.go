package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
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

	if err := SendWhatsAppTest(context.Background(), cfg, "Test Station"); err != nil {
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
	if got[0].MessagingProduct != whatsAppMessagingProduct || got[0].Type != whatsAppMessageText {
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
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token","code":190,"fbtrace_id":"trace-1"}}`))
	}))
	defer server.Close()

	whatsappAPIBaseURL = server.URL
	whatsappClient = server.Client()

	cfg := &WhatsAppConfig{
		PhoneNumberID: "12345",
		AccessToken:   "bad-token",
		Recipients:    "+31612345678",
	}

	err := SendWhatsAppTest(context.Background(), cfg, "Test Station")
	if err == nil {
		t.Fatal("SendWhatsAppTest() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "status 400") ||
		!strings.Contains(err.Error(), "Invalid OAuth access token") ||
		!strings.Contains(err.Error(), "trace: trace-1") {
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

	if err := SendWhatsAppTest(context.Background(), cfg, "Test Station"); err != nil {
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
	text := got.Template.Components[0].Parameters[0].Text
	if !strings.Contains(text, "Test WhatsApp notification") {
		t.Fatalf("template parameter text = %q, want test notification text", text)
	}
	if containsWhatsAppTemplateForbiddenWhitespace(text) {
		t.Fatalf("template parameter text = %q, contains forbidden whitespace", text)
	}
}

func TestWhatsAppChannelProductionMethodsUseTemplateSafeText(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	var mu sync.Mutex
	got := []whatsappTemplateRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload whatsappTemplateRequest
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

	ch := &WhatsAppChannel{}
	cfg := &config.Snapshot{
		StationName:              "Test Station",
		SilenceThreshold:         -40,
		WhatsAppPhoneNumberID:    "12345",
		WhatsAppAccessToken:      "test-token",
		WhatsAppRecipients:       "+31612345678",
		WhatsAppTemplateName:     "encoder_alert",
		WhatsAppTemplateLanguage: "",
	}

	calls := []struct {
		name string
		send func() error
		want string
	}{
		{
			name: "silence start",
			send: func() error {
				return ch.SendSilenceStart(context.Background(), cfg, -70.1, -69.2)
			},
			want: "Silence Detected",
		},
		{
			name: "silence end",
			send: func() error {
				return ch.SendSilenceEnd(context.Background(), cfg, 65000, -18.1, -17.2)
			},
			want: "Audio Restored",
		},
		{
			name: "audio dump",
			send: func() error {
				return ch.SendAudioDump(context.Background(), cfg, 65000, -18.1, -17.2, &silencedump.EncodeResult{
					Filename: "silence.mp3",
					FileSize: 12345,
				})
			},
			want: "silence.mp3",
		},
		{
			name: "upload abandoned",
			send: func() error {
				return ch.SendUploadAbandoned(context.Background(), cfg, UploadAbandonedData{
					RecorderName: "Recorder 1",
					Filename:     "hour.mp3",
					S3Key:        "recordings/hour.mp3",
					RetryCount:   3,
					LastError:    "network timeout",
				})
			},
			want: "Upload Abandoned",
		},
	}

	for _, tt := range calls {
		if err := tt.send(); err != nil {
			t.Fatalf("%s error = %v", tt.name, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(got) != len(calls) {
		t.Fatalf("request count = %d, want %d", len(got), len(calls))
	}
	for i, req := range got {
		if req.Type != whatsAppMessageTemplate {
			t.Fatalf("request %d Type = %q, want template", i, req.Type)
		}
		if req.Template.Language.Code != defaultWhatsAppTemplateLanguage {
			t.Fatalf("request %d language = %q, want default %q",
				i, req.Template.Language.Code, defaultWhatsAppTemplateLanguage)
		}
		text := req.Template.Components[0].Parameters[0].Text
		if !strings.Contains(text, calls[i].want) {
			t.Fatalf("request %d text = %q, want substring %q", i, text, calls[i].want)
		}
		if containsWhatsAppTemplateForbiddenWhitespace(text) {
			t.Fatalf("request %d text = %q, contains forbidden whitespace", i, text)
		}
	}
}

func TestWhatsAppChannelProductionMethodUsesFreeTextWithoutTemplate(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	var got whatsappTextRequest
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

	ch := &WhatsAppChannel{}
	cfg := &config.Snapshot{
		StationName:           "Test Station",
		SilenceThreshold:      -40,
		WhatsAppPhoneNumberID: "12345",
		WhatsAppAccessToken:   "test-token",
		WhatsAppRecipients:    "+31612345678",
	}

	if err := ch.SendSilenceStart(context.Background(), cfg, -70.1, -69.2); err != nil {
		t.Fatalf("SendSilenceStart() error = %v", err)
	}
	if got.Type != whatsAppMessageText {
		t.Fatalf("Type = %q, want text", got.Type)
	}
	if got.Text.Body == "" || !strings.Contains(got.Text.Body, "Silence Detected") {
		t.Fatalf("Text.Body = %q, want silence alert body", got.Text.Body)
	}
}

func TestValidateWhatsAppConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *WhatsAppConfig
		want string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: "configuration is required",
		},
		{
			name: "empty phone number id",
			cfg: &WhatsAppConfig{
				AccessToken: "token",
				Recipients:  "+31612345678",
			},
			want: "phone number ID is required",
		},
		{
			name: "whitespace access token",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   " \t ",
				Recipients:    "+31612345678",
			},
			want: "access token is required",
		},
		{
			name: "empty split recipients",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   "token",
				Recipients:    ",,, ",
			},
			want: "recipients are required",
		},
		{
			name: "invalid digits",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   "token",
				Recipients:    "+3161234567x",
			},
			want: "recipient",
		},
		{
			name: "template language without name",
			cfg: &WhatsAppConfig{
				PhoneNumberID:    "12345",
				AccessToken:      "token",
				Recipients:       "+31612345678",
				TemplateLanguage: "nl",
			},
			want: "template language requires template name",
		},
		{
			name: "invalid template name",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   "token",
				Recipients:    "+31612345678",
				TemplateName:  "Encoder Alert",
			},
			want: "template name is invalid",
		},
		{
			name: "template language whitespace",
			cfg: &WhatsAppConfig{
				PhoneNumberID:    "12345",
				AccessToken:      "token",
				Recipients:       "+31612345678",
				TemplateName:     "encoder_alert",
				TemplateLanguage: "nl NL",
			},
			want: "template language cannot contain whitespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := validateWhatsAppConfig(tt.cfg)
			if err == nil {
				t.Fatal("validateWhatsAppConfig() error = nil, want error")
			}
			if !errors.Is(err, ErrWhatsAppConfig) {
				t.Fatalf("validateWhatsAppConfig() error = %v, want ErrWhatsAppConfig", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateWhatsAppConfig() error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

func TestSendWhatsAppMessageAggregatesRecipientErrors(t *testing.T) {
	prevClient := whatsappClient
	prevBaseURL := whatsappAPIBaseURL
	t.Cleanup(func() {
		whatsappClient = prevClient
		whatsappAPIBaseURL = prevBaseURL
	})

	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		if count == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.test"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Bad recipient","code":131026}}`))
	}))
	defer server.Close()

	whatsappAPIBaseURL = server.URL
	whatsappClient = server.Client()

	err := SendWhatsAppTest(context.Background(), &WhatsAppConfig{
		PhoneNumberID: "12345",
		AccessToken:   "token",
		Recipients:    "+31612345678,+31687654321",
	}, "Test Station")
	if err == nil {
		t.Fatal("SendWhatsAppTest() error = nil, want partial failure")
	}
	var recipientErr *WhatsAppRecipientError
	if !errors.As(err, &recipientErr) {
		t.Fatalf("SendWhatsAppTest() error = %T, want WhatsAppRecipientError", err)
	}
	if recipientErr.RecipientCount != 2 || recipientErr.FailedCount != 1 {
		t.Fatalf("recipient counts = %d/%d, want 1 failed of 2",
			recipientErr.FailedCount, recipientErr.RecipientCount)
	}
	if !strings.Contains(err.Error(), "1 of 2 WhatsApp recipients failed") {
		t.Fatalf("SendWhatsAppTest() error = %q, want recipient counts", err.Error())
	}
}

func TestWhatsAppDumpDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result *silencedump.EncodeResult
		want   string
	}{
		{
			name:   "nil result",
			result: nil,
			want:   "not available",
		},
		{
			name: "encode error",
			result: &silencedump.EncodeResult{
				Error: errors.New("ffmpeg failed"),
			},
			want: "failed to capture",
		},
		{
			name:   "missing filename",
			result: &silencedump.EncodeResult{},
			want:   "no filename",
		},
		{
			name: "filename",
			result: &silencedump.EncodeResult{
				Filename: "silence.mp3",
				FileSize: 42,
			},
			want: "silence.mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := whatsAppDumpDetail(tt.result)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("whatsAppDumpDetail() = %q, want substring %q", got, tt.want)
			}
		})
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

	if ch.Name() != "whatsapp" {
		t.Fatalf("Name() = %q, want whatsapp", ch.Name())
	}
	if !ch.IsConfiguredForSilence(cfg) {
		t.Fatal("IsConfiguredForSilence() = false, want true")
	}
	if !ch.IsConfiguredForUpload(cfg) {
		t.Fatal("IsConfiguredForUpload() = false, want true")
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
	cfg.WhatsAppEvents.AudioDump = false
	if ch.SubscribesAudioDump(cfg) {
		t.Fatal("SubscribesAudioDump() = true, want false")
	}
}

func containsWhatsAppTemplateForbiddenWhitespace(text string) bool {
	return strings.ContainsAny(text, "\n\r\t") || strings.Contains(text, "     ")
}
