package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestWebhookChannelSendPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		send func(*WebhookChannel, *config.Snapshot) error
		want map[string]any
	}{
		{
			name: "silence start",
			send: func(channel *WebhookChannel, cfg *config.Snapshot) error {
				return channel.SendSilenceStart(context.Background(), cfg, -48.5, -52.3)
			},
			want: map[string]any{
				"event":          "silence_start",
				"level_left_db":  -48.5,
				"level_right_db": -52.3,
				"threshold":      -40.0,
			},
		},
		{
			name: "silence end",
			send: func(channel *WebhookChannel, cfg *config.Snapshot) error {
				return channel.SendSilenceEnd(context.Background(), cfg, 65000, -12.3, -14.1)
			},
			want: map[string]any{
				"event":               "silence_end",
				"silence_duration_ms": float64(65000),
				"level_left_db":       -12.3,
				"level_right_db":      -14.1,
				"threshold":           -40.0,
			},
		},
		{
			name: "upload abandoned",
			send: func(channel *WebhookChannel, cfg *config.Snapshot) error {
				return channel.SendUploadAbandoned(context.Background(), cfg, UploadAbandonedData{
					RecorderName: "Archive",
					Filename:     "archive.mp3",
					S3Key:        "recordings/Archive/archive.mp3",
					RetryCount:   24,
					LastError:    "request timed out",
				})
			},
			want: map[string]any{
				"event":         "upload_abandoned",
				"message":       "Upload abandoned for archive.mp3 after 24 retries: request timed out",
				"recorder_name": "Archive",
				"filename":      "archive.mp3",
				"s3_key":        "recordings/Archive/archive.mp3",
				"retry_count":   float64(24),
				"last_error":    "request timed out",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := captureWebhookPayload(t, func(webhookURL string) error {
				return tt.send(&WebhookChannel{}, &config.Snapshot{
					WebhookURL:       webhookURL,
					SilenceThreshold: -40,
				})
			})
			assertWebhookPayload(t, payload, tt.want)
		})
	}
}

func TestSendWebhookTestPayload(t *testing.T) {
	t.Parallel()

	payload := captureWebhookPayload(t, func(webhookURL string) error {
		return SendWebhookTest(webhookURL, "ZuidWest FM")
	})
	assertWebhookPayload(t, payload, map[string]any{
		"event":   "test",
		"message": "This is a test notification from ZuidWest FM",
	})
}

func TestWebhookChannelRejectsNonSuccessStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusMultipleChoices, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			err := (&WebhookChannel{}).SendSilenceStart(context.Background(), &config.Snapshot{
				WebhookURL: server.URL,
			}, -48, -52)
			if err == nil {
				t.Fatalf("SendSilenceStart() error = nil for status %d", status)
			}
		})
	}
}

func TestWebhookChannelSubscribesOnlyToEnabledEvents(t *testing.T) {
	t.Parallel()

	assertSubscribesOnlyToEnabledEvents(t, &WebhookChannel{}, true, func(events types.EventSubscriptions, configured bool) *config.Snapshot {
		cfg := &config.Snapshot{WebhookEvents: events}
		if configured {
			cfg.WebhookURL = "http://127.0.0.1/webhook"
		}
		return cfg
	})
}

func TestWebhookChannelUnconfiguredSendsNothing(t *testing.T) {
	t.Parallel()

	channel := &WebhookChannel{}
	cfg := &config.Snapshot{}
	tests := []struct {
		name string
		send func() error
	}{
		{name: "silence start", send: func() error {
			return channel.SendSilenceStart(context.Background(), cfg, -48, -52)
		}},
		{name: "silence end", send: func() error {
			return channel.SendSilenceEnd(context.Background(), cfg, 1000, -12, -14)
		}},
		{name: "channel imbalance start", send: func() error {
			return channel.SendChannelImbalanceStart(context.Background(), cfg, ChannelImbalanceData{})
		}},
		{name: "channel imbalance end", send: func() error {
			return channel.SendChannelImbalanceEnd(context.Background(), cfg, ChannelImbalanceData{})
		}},
		{name: "upload abandoned", send: func() error {
			return channel.SendUploadAbandoned(context.Background(), cfg, UploadAbandonedData{})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.send(); err != nil {
				t.Fatalf("unconfigured send returned error: %v", err)
			}
		})
	}
}

func captureWebhookPayload(t *testing.T, send func(string) error) map[string]any {
	t.Helper()

	payloads := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		payloads <- payload
	}))
	defer server.Close()

	if err := send(server.URL); err != nil {
		t.Fatalf("send webhook: %v", err)
	}

	select {
	case payload := <-payloads:
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook payload")
		return nil
	}
}

func assertWebhookPayload(t *testing.T, payload, want map[string]any) {
	t.Helper()

	for key, wantValue := range want {
		if got := payload[key]; got != wantValue {
			t.Errorf("payload[%q] = %v, want %v", key, got, wantValue)
		}
	}
	timestamp, ok := payload["timestamp"].(string)
	if !ok {
		t.Fatalf("payload timestamp = %v, want RFC3339 string", payload["timestamp"])
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		t.Errorf("payload timestamp %q is not RFC3339: %v", timestamp, err)
	}
}

func TestWebhookChannelImbalancePayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		send         func(context.Context, string, ChannelImbalanceData) error
		data         ChannelImbalanceData
		wantEvent    string
		wantDuration float64
	}{
		{
			name:      "start",
			send:      sendWebhookChannelImbalanceStart,
			wantEvent: "channel_imbalance_start",
			data: ChannelImbalanceData{
				LevelL:      -6,
				LevelR:      -30,
				BalanceDB:   24,
				ImbalanceDB: 24,
				ThresholdDB: 12,
			},
		},
		{
			name:      "end serializes zero balance",
			send:      sendWebhookChannelImbalanceEnd,
			wantEvent: "channel_imbalance_end",
			data: ChannelImbalanceData{
				LevelL:      -8,
				LevelR:      -8,
				BalanceDB:   0,
				ImbalanceDB: 0,
				ThresholdDB: 12,
				DurationMs:  16000,
			},
			wantDuration: 16000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payloadCh := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode webhook payload: %v", err)
					payloadCh <- nil
					return
				}
				payloadCh <- payload
			}))
			defer server.Close()

			if err := tt.send(context.Background(), server.URL, tt.data); err != nil {
				t.Fatalf("send webhook: %v", err)
			}

			payload := <-payloadCh
			if payload["event"] != tt.wantEvent {
				t.Fatalf("event = %v, want %s", payload["event"], tt.wantEvent)
			}
			if payload["threshold"] != tt.data.ThresholdDB {
				t.Fatalf("threshold = %v, want %.1f", payload["threshold"], tt.data.ThresholdDB)
			}
			if payload["balance_db"] != tt.data.BalanceDB {
				t.Fatalf("balance_db = %v, want %.1f", payload["balance_db"], tt.data.BalanceDB)
			}
			if payload["imbalance_db"] != tt.data.ImbalanceDB {
				t.Fatalf("imbalance_db = %v, want %.1f", payload["imbalance_db"], tt.data.ImbalanceDB)
			}
			if tt.wantDuration == 0 {
				if _, ok := payload["duration_ms"]; ok {
					t.Fatal("duration_ms present on start payload")
				}
			} else if payload["duration_ms"] != tt.wantDuration {
				t.Fatalf("duration_ms = %v, want %.0f", payload["duration_ms"], tt.wantDuration)
			}
		})
	}
}

func TestWebhookChannelImbalanceDumpPayload(t *testing.T) {
	t.Parallel()
	payloadCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
			payloadCh <- nil
			return
		}
		payloadCh <- payload
	}))
	defer server.Close()

	balanceDB, imbalanceDB := 1.0, 1.0
	data := AudioDumpData{
		Trigger:     silencedump.TriggerChannelImbalance,
		LevelL:      -12,
		LevelR:      -13,
		BalanceDB:   &balanceDB,
		ImbalanceDB: &imbalanceDB,
		ThresholdDB: 12,
		DurationMs:  30000,
	}
	if err := sendWebhookDumpReady(context.Background(), server.URL, &data); err != nil {
		t.Fatalf("send webhook dump: %v", err)
	}

	payload := <-payloadCh
	if payload["event"] != "audio_dump_ready" || payload["trigger"] != "channel_imbalance" {
		t.Fatalf("event identity = %v/%v, want audio_dump_ready/channel_imbalance", payload["event"], payload["trigger"])
	}
	if payload["duration_ms"] != float64(30000) || payload["threshold"] != float64(12) {
		t.Fatalf("dump timing/threshold payload = %+v", payload)
	}
	if payload["balance_db"] != float64(1) || payload["imbalance_db"] != float64(1) {
		t.Fatalf("dump imbalance payload = %+v", payload)
	}
	if _, ok := payload["silence_duration_ms"]; ok {
		t.Fatal("imbalance dump unexpectedly contains silence_duration_ms")
	}
}
