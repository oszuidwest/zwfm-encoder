package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
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
func sendWebhookSilence(ctx context.Context, webhookURL string, e silenceEventData) error {
	return sendWebhook(ctx, webhookURL, &WebhookPayload{
		Event:        "silence_start",
		LevelLeftDB:  e.LevelL,
		LevelRightDB: e.LevelR,
		Threshold:    e.Threshold,
		Timestamp:    timestampUTC(),
	})
}

// sendWebhookSilenceEnd notifies the configured webhook that silence has ended.
func sendWebhookSilenceEnd(ctx context.Context, webhookURL string, e silenceEventData) error {
	return sendWebhook(ctx, webhookURL, &WebhookPayload{
		Event:             "silence_end",
		SilenceDurationMs: e.DurationMs,
		LevelLeftDB:       e.LevelL,
		LevelRightDB:      e.LevelR,
		Threshold:         e.Threshold,
		Timestamp:         timestampUTC(),
	})
}

// SendWebhookTest sends a test webhook notification.
func SendWebhookTest(webhookURL, stationName string) error {
	if err := formatWebhookRuntimeError(types.ValidateWebhookURL(webhookURL, validation.RequireComplete)); err != nil {
		return err
	}

	return sendWebhook(context.Background(), webhookURL, &WebhookPayload{
		Event:     "test",
		Message:   "This is a test notification from " + stationName,
		Timestamp: timestampUTC(),
	})
}

func formatWebhookRuntimeError(issues validation.Issues) error {
	if len(issues) == 0 {
		return nil
	}
	switch types.WebhookValidationCode(issues[0].Code) {
	case types.WebhookURLRequired:
		return fmt.Errorf("webhook URL not configured")
	case types.WebhookURLInvalid:
		return fmt.Errorf("webhook URL is invalid")
	default:
		return fmt.Errorf("invalid webhook configuration")
	}
}

// sendUploadAbandonedWebhook sends an upload_abandoned event to the webhook endpoint.
func sendUploadAbandonedWebhook(ctx context.Context, webhookURL string, p UploadAbandonedData) error {
	return sendWebhook(ctx, webhookURL, &WebhookPayload{
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
func sendWebhookDumpReady(ctx context.Context, webhookURL string, e silenceEventData) error {
	payload := &WebhookPayload{
		Event:             "audio_dump_ready",
		SilenceDurationMs: e.DurationMs,
		LevelLeftDB:       e.LevelL,
		LevelRightDB:      e.LevelR,
		Threshold:         e.Threshold,
		Timestamp:         timestampUTC(),
	}

	if e.Dump != nil {
		if e.Dump.Error != nil {
			payload.AudioDumpError = e.Dump.Error.Error()
		} else if e.Dump.FilePath != "" {
			// Read and encode the dump file
			data, err := os.ReadFile(e.Dump.FilePath)
			if err != nil {
				payload.AudioDumpError = err.Error()
			} else {
				payload.AudioDumpBase64 = base64.StdEncoding.EncodeToString(data)
				payload.AudioDumpFilename = e.Dump.Filename
				payload.AudioDumpSizeBytes = e.Dump.FileSize
			}
		}
	}

	return sendWebhook(ctx, webhookURL, payload)
}

// sendWebhook delivers a notification to the configured webhook endpoint.
func sendWebhook(ctx context.Context, webhookURL string, payload *WebhookPayload) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !util.IsConfigured(webhookURL) {
		return nil // Silently skip if not configured
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal payload", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return util.WrapError("create webhook request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := webhookClient.Do(req)
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

func (c *WebhookChannel) SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error {
	return sendWebhookSilence(ctx, cfg.WebhookURL, silenceEventData{
		LevelL:    levelL,
		LevelR:    levelR,
		Threshold: cfg.SilenceThreshold,
	})
}

func (c *WebhookChannel) SendSilenceEnd(
	ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
) error {
	return sendWebhookSilenceEnd(ctx, cfg.WebhookURL, silenceEventData{
		DurationMs: durationMS,
		LevelL:     levelL,
		LevelR:     levelR,
		Threshold:  cfg.SilenceThreshold,
	})
}

func (c *WebhookChannel) SendAudioDump(
	ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
	result *silencedump.EncodeResult,
) error {
	return sendWebhookDumpReady(ctx, cfg.WebhookURL, silenceEventData{
		DurationMs: durationMS,
		LevelL:     levelL,
		LevelR:     levelR,
		Threshold:  cfg.SilenceThreshold,
		Dump:       result,
	})
}

func (c *WebhookChannel) SendUploadAbandoned(ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData) error {
	return sendUploadAbandonedWebhook(ctx, cfg.WebhookURL, params)
}
