package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// WebhookPayload represents the data sent to webhook endpoints.
type WebhookPayload struct {
	Event             string  `json:"event"`
	SilenceDurationMs int64   `json:"silence_duration_ms,omitempty"`
	LevelLeftDB       float64 `json:"level_left_db,omitempty"`  // dB
	LevelRightDB      float64 `json:"level_right_db,omitempty"` // dB
	Threshold         float64 `json:"threshold,omitempty"`      // dB
	Message           string  `json:"message,omitempty"`
	Timestamp         string  `json:"timestamp"` // RFC3339

	AudioDumpBase64    string `json:"audio_dump_base64,omitempty"`
	AudioDumpFilename  string `json:"audio_dump_filename,omitempty"`
	AudioDumpSizeBytes int64  `json:"audio_dump_size_bytes,omitempty"`
	AudioDumpError     string `json:"audio_dump_error,omitempty"`
}

// SendWebhookSilence notifies the configured webhook of critical silence detection.
func SendWebhookSilence(webhookURL string, levelL, levelR, threshold float64) error {
	return sendWebhook(webhookURL, &WebhookPayload{
		Event:        "silence_detected",
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		Threshold:    threshold,
		Timestamp:    timestampUTC(),
	})
}

// SendWebhookTest sends a test webhook notification.
func SendWebhookTest(webhookURL, stationName string) error {
	if webhookURL == "" {
		return fmt.Errorf("webhook URL not configured")
	}

	return sendWebhook(webhookURL, &WebhookPayload{
		Event:     "test",
		Message:   "This is a test notification from " + stationName,
		Timestamp: timestampUTC(),
	})
}

// sendWebhook delivers a notification to the configured webhook endpoint.
func sendWebhook(webhookURL string, payload *WebhookPayload) error {
	if !util.IsConfigured(webhookURL) {
		return nil // Silently skip if not configured
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal payload", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return util.WrapError("send webhook request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}
