package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// WebhookPayload represents the JSON structure sent to webhook endpoints.
type WebhookPayload struct {
	Event           string  `json:"event"`
	SilenceDuration float64 `json:"silence_duration,omitempty"`
	Threshold       float64 `json:"threshold,omitempty"`
	Message         string  `json:"message,omitempty"`
	Timestamp       string  `json:"timestamp"`
}

// SendSilenceWebhook notifies the configured webhook of critical silence detection.
func SendSilenceWebhook(webhookURL string, duration, threshold float64) error {
	return sendWebhook(webhookURL, WebhookPayload{
		Event:           "silence_detected",
		SilenceDuration: duration,
		Threshold:       threshold,
		Timestamp:       timestampUTC(),
	})
}

// SendRecoveryWebhook notifies the configured webhook of audio recovery.
func SendRecoveryWebhook(webhookURL string, silenceDuration float64) error {
	return sendWebhook(webhookURL, WebhookPayload{
		Event:           "silence_recovered",
		SilenceDuration: silenceDuration,
		Timestamp:       timestampUTC(),
	})
}

// SendTestWebhook verifies webhook configuration by sending a test notification.
func SendTestWebhook(webhookURL string) error {
	if webhookURL == "" {
		return fmt.Errorf("webhook URL not configured")
	}

	return sendWebhook(webhookURL, WebhookPayload{
		Event:     "test",
		Message:   "This is a test notification from " + AppName,
		Timestamp: timestampUTC(),
	})
}

// sendWebhook delivers a notification to the configured webhook endpoint.
func sendWebhook(webhookURL string, payload WebhookPayload) error {
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
	defer util.SafeCloseFunc(resp.Body, "webhook response body")()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}
