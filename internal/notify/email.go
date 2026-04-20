package notify

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig is the configuration for email notifications.
type GraphConfig = types.GraphConfig

// EmailChannel implements AlertChannel for Microsoft Graph email delivery.
type EmailChannel struct {
	mu           sync.Mutex
	cachedClient *GraphClient
	cachedKey    string
}

// sendUploadAbandonedEmail sends an upload abandonment alert via Microsoft Graph.
// A fresh client is created each time because upload abandonments are rare one-off events
// where caching would add complexity without meaningful benefit.
func sendUploadAbandonedEmail(cfg *GraphConfig, stationName string, p UploadAbandonedData) error {
	client, err := NewGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	subject := "[ALERT] Upload Abandoned - " + stationName
	body := fmt.Sprintf(
		"A recording upload was abandoned at %s.\n\n"+
			"Recorder: %s\n"+
			"File: %s\n"+
			"S3 key: %s\n"+
			"Retries: %d\n"+
			"Last error: %s\n\n"+
			"The file could not be uploaded to S3 after exhausting all retries.",
		util.HumanTime(), p.RecorderName, p.Filename, p.S3Key, p.RetryCount, p.LastError,
	)

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	if err := client.SendMail(recipients, subject, body); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}

func sendEmailWithClient(cfg *GraphConfig, client *GraphClient, subject, body string) error {
	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	if err := client.SendMail(recipients, subject, body); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}

func sendSilenceEmailWithClient(cfg *GraphConfig, client *GraphClient, stationName string, levelL, levelR, threshold float64) error {
	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"The encoder detected silence at %s.\n\n"+
			"Audio level dropped below the %.0f dB threshold.\n"+
			"Current level: Left %.1f dB / Right %.1f dB\n\n"+
			"Silence is ongoing. Please check the audio source.",
		util.HumanTime(), threshold, levelL, levelR,
	)
	return sendEmailWithClient(cfg, client, subject, body)
}

func sendSilenceEndEmailWithClient(cfg *GraphConfig, client *GraphClient, stationName string, durationMs int64, levelL, levelR, threshold float64) error {
	subject := "[OK] Audio Restored - " + stationName
	body := fmt.Sprintf(
		"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		util.HumanTime(), util.FormatDuration(durationMs), levelL, levelR, threshold,
	)
	return sendEmailWithClient(cfg, client, subject, body)
}

// sendDumpReadyEmailWithClient does not delegate to sendEmailWithClient because it needs to attach a file.
func sendDumpReadyEmailWithClient(cfg *GraphConfig, client *GraphClient, stationName string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	subject := "[DUMP] Audio Recording - " + stationName

	body := fmt.Sprintf(
		"Audio dump ready at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		util.HumanTime(), util.FormatDuration(durationMs), levelL, levelR, threshold,
	)

	if dump != nil && dump.Error != nil {
		body += fmt.Sprintf("\n\nAudio recording: Failed to capture (%s)", dump.Error.Error())
	}

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	// Prepare attachment if dump is available.
	var attachment *EmailAttachment
	if dump != nil && dump.Error == nil && dump.FilePath != "" {
		data, err := os.ReadFile(dump.FilePath)
		if err == nil {
			attachment = &EmailAttachment{
				Filename:    dump.Filename,
				ContentType: "audio/mpeg",
				Data:        data,
			}
			body += "\n\nAudio recording attached (15s before and after the silence)."
		} else {
			slog.Warn("audio dump file unreadable, sending email without attachment", "path", dump.FilePath, "error", err)
			body += fmt.Sprintf("\n\nAudio recording unavailable (file could not be read: %s).", err)
		}
	}

	if err := client.SendMailWithAttachment(recipients, subject, body, attachment); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}

func graphConfigKeyFrom(cfg *GraphConfig) string {
	return cfg.TenantID + "|" + cfg.ClientID + "|" + cfg.ClientSecret + "|" + cfg.FromAddress
}

// Name returns the channel identifier used in logs.
func (c *EmailChannel) Name() string { return "email" }

// IsConfiguredForSilence reports whether the channel participates in silence flows.
func (c *EmailChannel) IsConfiguredForSilence(cfg *config.Snapshot) bool { return cfg.HasGraph() }

// IsConfiguredForUpload reports whether the channel participates in upload-abandonment flows.
func (c *EmailChannel) IsConfiguredForUpload(cfg *config.Snapshot) bool { return cfg.HasGraph() }

// SubscribesSilenceStart reports whether silence-start events should be sent.
func (c *EmailChannel) SubscribesSilenceStart(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.SilenceStart
}

// SubscribesSilenceEnd reports whether silence-end events should be sent.
func (c *EmailChannel) SubscribesSilenceEnd(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.SilenceEnd
}

// SubscribesAudioDump reports whether audio-dump events should be sent.
func (c *EmailChannel) SubscribesAudioDump(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.AudioDump
}

// getOrCreateClient returns a cached Graph client for the given config, creating one only when the
// config changes. Caching avoids repeated token fetches during silence events where start and
// recovery emails are sent in quick succession.
func (c *EmailChannel) getOrCreateClient(graphCfg *GraphConfig) (*GraphClient, error) {
	key := graphConfigKeyFrom(graphCfg)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cachedClient != nil && c.cachedKey == key {
		return c.cachedClient, nil
	}

	client, err := NewGraphClient(graphCfg)
	if err != nil {
		return nil, err
	}

	c.cachedClient = client
	c.cachedKey = key
	return client, nil
}

func (c *EmailChannel) SendSilenceStart(cfg *config.Snapshot, levelL, levelR float64) error {
	graphCfg := BuildGraphConfig(cfg)
	client, err := c.getOrCreateClient(graphCfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}
	return sendSilenceEmailWithClient(graphCfg, client, cfg.StationName, levelL, levelR, cfg.SilenceThreshold)
}

func (c *EmailChannel) SendSilenceEnd(cfg *config.Snapshot, durationMs int64, levelL, levelR float64) error {
	graphCfg := BuildGraphConfig(cfg)
	client, err := c.getOrCreateClient(graphCfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}
	return sendSilenceEndEmailWithClient(graphCfg, client, cfg.StationName, durationMs, levelL, levelR, cfg.SilenceThreshold)
}

func (c *EmailChannel) SendAudioDump(cfg *config.Snapshot, durationMs int64, levelL, levelR float64, result *silencedump.EncodeResult) error {
	graphCfg := BuildGraphConfig(cfg)
	client, err := c.getOrCreateClient(graphCfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}
	return sendDumpReadyEmailWithClient(graphCfg, client, cfg.StationName, durationMs, levelL, levelR, cfg.SilenceThreshold, result)
}

func (c *EmailChannel) SendUploadAbandoned(cfg *config.Snapshot, params UploadAbandonedData) error {
	return sendUploadAbandonedEmail(BuildGraphConfig(cfg), cfg.StationName, params)
}

// SendTestEmail sends a test email to verify email configuration.
func SendTestEmail(cfg *GraphConfig, stationName string) error {
	if err := ValidateConfig(cfg); err != nil {
		return util.WrapError("validate configuration", err)
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	// Validate authentication first
	if err := client.ValidateAuth(); err != nil {
		return util.WrapError("authenticate", err)
	}

	subject := "[TEST] " + stationName
	body := fmt.Sprintf(
		"Test email from the audio encoder.\n\n"+
			"Time: %s\n\n"+
			"Microsoft Graph configuration is working correctly.",
		util.HumanTime(),
	)

	recipients := ParseRecipients(cfg.Recipients)
	if err := client.SendMail(recipients, subject, body); err != nil {
		return util.WrapError("send email", err)
	}

	return nil
}
