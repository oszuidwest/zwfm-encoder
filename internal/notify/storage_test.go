package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecordingStorageWebhookAndEmail(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		low     bool
		problem string
		event   string
	}{
		{"low", true, "", "recording_storage_low"},
		{"unavailable", false, "offline", "recording_storage_low"},
		{"recovered", false, "", "recording_storage_restored"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payloads := make(chan WebhookPayload, 1)
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				var payload WebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				payloads <- payload
			}))
			defer server.Close()
			paths := []types.RecordingStorageStatus{{Path: "/spool", Low: tt.low, Error: tt.problem, ThresholdBytes: 1 << 30}}
			if err := (&WebhookChannel{}).SendRecordingStorage(t.Context(), &config.Snapshot{WebhookURL: server.URL}, paths); err != nil {
				t.Fatal(err)
			}
			payload := awaitCall(t, payloads, "storage webhook")
			if payload.Event != tt.event || len(payload.StoragePaths) != 1 || payload.StoragePaths[0].AvailableBytes != 0 || payload.StoragePaths[0].Error != tt.problem {
				t.Fatalf("payload=%+v", payload)
			}
			subject, body := recordingStorageEmail("Station", paths)
			if strings.HasPrefix(subject, "[ALERT]") != (tt.event == "recording_storage_low") || !strings.Contains(body, "/spool: 0 bytes available") || !strings.Contains(body, "No recordings have been automatically evicted") {
				t.Fatalf("email=%q %q", subject, body)
			}
		})
	}
}

type storageTestChannel struct {
	noopAlertChannel
	configured bool
	entered    chan struct{}
	release    chan struct{}
	calls      chan bool
}

func (c *storageTestChannel) IsConfiguredForUpload(*config.Snapshot) bool { return c.configured }
func (c *storageTestChannel) SendRecordingStorage(_ context.Context, _ *config.Snapshot, paths []types.RecordingStorageStatus) error {
	if paths[0].Low {
		close(c.entered)
		<-c.release
	}
	c.calls <- paths[0].Low
	return nil
}

func TestRecordingStorageAlertsOrderedAndIndependentOfSubscriptions(t *testing.T) {
	t.Parallel()
	ch := &storageTestChannel{configured: true, entered: make(chan struct{}), release: make(chan struct{}), calls: make(chan bool, 2)}
	unconfigured := &storageTestChannel{entered: make(chan struct{}), calls: make(chan bool, 2)}
	o := NewAlertOrchestrator(config.New(""), NewDispatcher(ch, unconfigured))
	t.Cleanup(o.Close)
	paths := []types.RecordingStorageStatus{{Path: "/spool", Low: true}}
	o.HandleRecordingStorage(paths)
	awaitCall(t, ch.entered, "low-space delivery started")
	paths[0].Low = false
	o.HandleRecordingStorage(paths)
	assertNoCall(t, ch.calls, "recovery overtook blocked low-space delivery")
	close(ch.release)
	if !awaitCall(t, ch.calls, "low-space alert") || awaitCall(t, ch.calls, "recovery alert") {
		t.Fatal("storage alerts were reordered or caller's mutation leaked into queued state")
	}
	for _, queue := range o.inputQueues {
		done := make(chan struct{})
		queue <- func() { close(done) }
		awaitCall(t, done, "delivery barrier")
	}
	assertNoCall(t, unconfigured.calls, "unconfigured storage channel")
}
