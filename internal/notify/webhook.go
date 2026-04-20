package notify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

var webhookClient = &http.Client{Timeout: 10 * time.Second}

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

	RecorderName string `json:"recorder_name,omitempty"`
	Filename     string `json:"filename,omitempty"`
	S3Key        string `json:"s3_key,omitempty"`
	RetryCount   int    `json:"retry_count,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// sendWebhookSilence notifies the configured webhook of critical silence detection.
func sendWebhookSilence(webhookURL string, levelL, levelR, threshold float64) error {
	return sendWebhook(webhookURL, &WebhookPayload{
		Event:        "silence_start",
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		Threshold:    threshold,
		Timestamp:    timestampUTC(),
	})
}

// sendWebhookSilenceEnd notifies the configured webhook that silence has ended.
func sendWebhookSilenceEnd(webhookURL string, durationMs int64, levelL, levelR, threshold float64) error {
	return sendWebhook(webhookURL, &WebhookPayload{
		Event:             "silence_end",
		SilenceDurationMs: durationMs,
		LevelLeftDB:       levelL,
		LevelRightDB:      levelR,
		Threshold:         threshold,
		Timestamp:         timestampUTC(),
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

// sendUploadAbandonedWebhook sends an upload_abandoned event to the webhook endpoint.
func sendUploadAbandonedWebhook(webhookURL string, p UploadAbandonedData) error {
	return sendWebhook(webhookURL, &WebhookPayload{
		Event:        "upload_abandoned",
		Message:      fmt.Sprintf("Upload abandoned for %s after %d retries: %s", p.Filename, p.RetryCount, p.LastError),
		Timestamp:    timestampUTC(),
		RecorderName: p.RecorderName,
		Filename:     p.Filename,
		S3Key:        p.S3Key,
		RetryCount:   p.RetryCount,
		LastError:    p.LastError,
	})
}

// sendWebhookDumpReady notifies the configured webhook that an audio dump is ready.
func sendWebhookDumpReady(webhookURL string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	payload := &WebhookPayload{
		Event:             "audio_dump_ready",
		SilenceDurationMs: durationMs,
		LevelLeftDB:       levelL,
		LevelRightDB:      levelR,
		Threshold:         threshold,
		Timestamp:         timestampUTC(),
	}

	if dump != nil {
		if dump.Error != nil {
			payload.AudioDumpError = dump.Error.Error()
		} else if dump.FilePath != "" {
			// Read and encode the dump file
			data, err := os.ReadFile(dump.FilePath)
			if err != nil {
				payload.AudioDumpError = err.Error()
			} else {
				payload.AudioDumpBase64 = base64.StdEncoding.EncodeToString(data)
				payload.AudioDumpFilename = dump.Filename
				payload.AudioDumpSizeBytes = dump.FileSize
			}
		}
	}

	return sendWebhook(webhookURL, payload)
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

	resp, err := webhookClient.Post(webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return util.WrapError("send webhook request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}

// WebhookChannel implements AlertChannel for HTTP webhook delivery.
type WebhookChannel struct{}

// Name returns the channel identifier used in logs.
func (c *WebhookChannel) Name() string { return "webhook" }

// IsConfiguredForSilence reports whether the channel participates in silence flows.
func (c *WebhookChannel) IsConfiguredForSilence(cfg *config.Snapshot) bool { return cfg.HasWebhook() }

// IsConfiguredForUpload reports whether the channel participates in upload-abandonment flows.
func (c *WebhookChannel) IsConfiguredForUpload(cfg *config.Snapshot) bool { return cfg.HasWebhook() }

// SubscribesSilenceStart reports whether silence-start events should be sent.
func (c *WebhookChannel) SubscribesSilenceStart(cfg *config.Snapshot) bool {
	return cfg.HasWebhook() && cfg.WebhookEvents.SilenceStart
}

// SubscribesSilenceEnd reports whether silence-end events should be sent.
func (c *WebhookChannel) SubscribesSilenceEnd(cfg *config.Snapshot) bool {
	return cfg.HasWebhook() && cfg.WebhookEvents.SilenceEnd
}

// SubscribesAudioDump reports whether audio-dump events should be sent.
func (c *WebhookChannel) SubscribesAudioDump(cfg *config.Snapshot) bool {
	return cfg.HasWebhook() && cfg.WebhookEvents.AudioDump
}

func (c *WebhookChannel) SendSilenceStart(cfg *config.Snapshot, levelL, levelR float64) error {
	return sendWebhookSilence(cfg.WebhookURL, levelL, levelR, cfg.SilenceThreshold)
}

func (c *WebhookChannel) SendSilenceEnd(cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error {
	return sendWebhookSilenceEnd(cfg.WebhookURL, durationMS, levelL, levelR, cfg.SilenceThreshold)
}

func (c *WebhookChannel) SendAudioDump(cfg *config.Snapshot, durationMS int64, levelL, levelR float64, result *silencedump.EncodeResult) error {
	return sendWebhookDumpReady(cfg.WebhookURL, durationMS, levelL, levelR, cfg.SilenceThreshold, result)
}

func (c *WebhookChannel) SendUploadAbandoned(cfg *config.Snapshot, params UploadAbandonedData) error {
	return sendUploadAbandonedWebhook(cfg.WebhookURL, params)
}
