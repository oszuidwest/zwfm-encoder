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
	// Event is the event type (silence_detected, silence_recovered, test).
	Event string `json:"event"`
	// SilenceDurationMs is how long the silence lasted in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms,omitempty"`
	// LevelLeftDB is the left channel audio level in dB when silence was detected.
	LevelLeftDB float64 `json:"level_left_db,omitempty"`
	// LevelRightDB is the right channel audio level in dB when silence was detected.
	LevelRightDB float64 `json:"level_right_db,omitempty"`
	// Threshold is the configured silence threshold in dB.
	Threshold float64 `json:"threshold,omitempty"`
	// Message is a human-readable description of the event.
	Message string `json:"message,omitempty"`
	// Timestamp is when the event occurred in RFC3339 format.
	Timestamp string `json:"timestamp"`

	// AudioDumpBase64 is the silence audio dump as base64-encoded MP3 data.
	AudioDumpBase64 string `json:"audio_dump_base64,omitempty"`
	// AudioDumpFilename is the audio dump file name (e.g., "2024-01-15_14-32-05.mp3").
	AudioDumpFilename string `json:"audio_dump_filename,omitempty"`
	// AudioDumpSizeBytes is the audio dump file size in bytes.
	AudioDumpSizeBytes int64 `json:"audio_dump_size_bytes,omitempty"`
	// AudioDumpError is the error message if dump encoding failed.
	AudioDumpError string `json:"audio_dump_error,omitempty"`
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

	client := &http.Client{Timeout: 10000 * time.Millisecond}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return util.WrapError("send webhook request", err)
	}
	defer util.SafeCloseFunc(resp.Body, "webhook response body")()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}
