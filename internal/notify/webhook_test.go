package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

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
					t.Fatalf("method = %s, want POST", r.Method)
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("decode webhook payload: %v", err)
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
			if _, ok := payload["threshold_db"]; ok {
				t.Fatal("payload contains threshold_db; want existing threshold field only")
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
