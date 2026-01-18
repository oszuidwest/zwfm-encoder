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
	// Event is the webhook event name.
	Event string `json:"event"`
	// SilenceDurationMs is the silence duration in milliseconds.
	SilenceDurationMs int64 `json:"silence_duration_ms,omitempty"`
	// LevelLeftDB is the left channel level in dB.
	LevelLeftDB float64 `json:"level_left_db,omitempty"`
	// LevelRightDB is the right channel level in dB.
	LevelRightDB float64 `json:"level_right_db,omitempty"`
	// Threshold is the silence threshold in dB.
	Threshold float64 `json:"threshold,omitempty"`
	// Message is a human-readable message.
	Message string `json:"message,omitempty"`
	// Timestamp is the event timestamp.
	Timestamp string `json:"timestamp"`

	// AudioDumpBase64 is the base64-encoded MP3 dump.
	AudioDumpBase64 string `json:"audio_dump_base64,omitempty"`
	// AudioDumpFilename is the dump file name.
	AudioDumpFilename string `json:"audio_dump_filename,omitempty"`
	// AudioDumpSizeBytes is the dump file size in bytes.
	AudioDumpSizeBytes int64 `json:"audio_dump_size_bytes,omitempty"`
	// AudioDumpError is the dump error message, if any.
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
