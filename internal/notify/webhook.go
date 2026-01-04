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
	LevelLeftDB       float64 `json:"level_left_db,omitempty"`
	LevelRightDB      float64 `json:"level_right_db,omitempty"`
	Threshold         float64 `json:"threshold,omitempty"`
	Message           string  `json:"message,omitempty"`
	Timestamp         string  `json:"timestamp"`

	// Audio dump fields (silence_recovered only)
	AudioDumpBase64    string `json:"audio_dump_base64,omitempty"`     // Base64-encoded MP3 dump
	AudioDumpFilename  string `json:"audio_dump_filename,omitempty"`   // Dump filename
	AudioDumpSizeBytes int64  `json:"audio_dump_size_bytes,omitempty"` // Dump file size in bytes
	AudioDumpError     string `json:"audio_dump_error,omitempty"`      // Error message if dump encoding failed
}

// SendSilenceWebhook notifies the configured webhook of critical silence detection.
func SendSilenceWebhook(webhookURL string, levelL, levelR, threshold float64) error {
	return sendWebhook(webhookURL, &WebhookPayload{
		Event:        "silence_detected",
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		Threshold:    threshold,
		Timestamp:    timestampUTC(),
	})
}

// SendTestWebhook sends a test webhook notification.
func SendTestWebhook(webhookURL, stationName string) error {
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
