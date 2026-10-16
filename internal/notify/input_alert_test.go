package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormatZabbixInputValues(t *testing.T) {
	t.Parallel()
	// Names chosen to collide with the template's substring triggers.
	data := InputData{Input: "dante://SILENCE-RECOVERY/01/02", Error: "TEST device gone", DurationMs: 42000}
	lost := formatZabbixInputLostValue(data)
	if lost != "event=INPUT_LOST" {
		t.Fatalf("lost value = %q, want %q", lost, "event=INPUT_LOST")
	}
	restored := formatZabbixInputRestoredValue(data)
	if want := "event=INPUT_RESTORED duration_ms=42000"; restored != want {
		t.Fatalf("restored value = %q, want %q", restored, want)
	}
	// The template's silence, recovery and test triggers match on these substrings.
	for _, value := range []string{lost, restored} {
		for _, marker := range []string{"SILENCE", "RECOVERY", "TEST"} {
			if strings.Contains(value, marker) {
				t.Fatalf("value %q contains %q and would fire a silence-item trigger", value, marker)
			}
		}
	}
}

func TestWebhookInputPayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		send func(context.Context, string, InputData) error
		want map[string]any
	}{
		{
			name: "lost",
			send: sendWebhookInputLost,
			want: map[string]any{"event": "input_lost", "input": "hw:1", "last_error": "device gone"},
		},
		{
			name: "restored",
			send: sendWebhookInputRestored,
			want: map[string]any{"event": "input_restored", "input": "hw:1", "duration_ms": float64(42000)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payloads := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode webhook payload: %v", err)
				}
				payloads <- payload
			}))
			defer server.Close()

			data := InputData{Input: "hw:1", Error: "device gone", DurationMs: 42000}
			if err := tt.send(context.Background(), server.URL, data); err != nil {
				t.Fatalf("send webhook: %v", err)
			}
			payload := awaitCall(t, payloads, "webhook payload")
			for key, want := range tt.want {
				if payload[key] != want {
					t.Errorf("payload[%q] = %v, want %v", key, payload[key], want)
				}
			}
		})
	}
}
