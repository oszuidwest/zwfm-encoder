package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig aliases the shared API shape used for Microsoft Graph email delivery.
type GraphConfig = types.GraphConfig

// EmailChannel delivers AlertChannel events through Microsoft Graph.
type EmailChannel struct {
	mu           sync.Mutex
	cachedClient *GraphClient
	cachedKey    string
}

// sendUploadAbandonedEmail sends an upload abandonment alert via Microsoft Graph.
// A fresh client is created each time because upload abandonments are rare one-off events
// where caching would add complexity without meaningful benefit.
func sendUploadAbandonedEmail(ctx context.Context, cfg *GraphConfig, stationName string, p UploadAbandonedData) error {
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

	return sendEmailWithClient(ctx, cfg, client, subject, body)
}

func sendEmailWithClient(ctx context.Context, cfg *GraphConfig, client *GraphClient, subject, body string) error {
	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	if err := client.SendMail(ctx, recipients, subject, body); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}

func sendSilenceEmailWithClient(
	ctx context.Context, cfg *GraphConfig, client *GraphClient, stationName string, e silenceEventData,
) error {
	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"The encoder detected silence at %s.\n\n"+
			"Audio level dropped below the %.0f dB threshold.\n"+
			"Current level: Left %.1f dB / Right %.1f dB\n\n"+
			"Silence is ongoing. Please check the audio source.",
		util.HumanTime(), e.Threshold, e.LevelL, e.LevelR,
	)
	return sendEmailWithClient(ctx, cfg, client, subject, body)
}

func sendSilenceEndEmailWithClient(
	ctx context.Context, cfg *GraphConfig, client *GraphClient, stationName string, e silenceEventData,
) error {
	subject := "[OK] Audio Restored - " + stationName
	body := fmt.Sprintf(
		"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		util.HumanTime(), util.FormatDuration(e.DurationMs), e.LevelL, e.LevelR, e.Threshold,
	)
	return sendEmailWithClient(ctx, cfg, client, subject, body)
}

func channelImbalanceDirection(balanceDB float64) string {
	switch {
	case balanceDB > 0:
		return "left louder"
	case balanceDB < 0:
		return "right louder"
	default:
		return "balanced"
	}
}

func buildChannelImbalanceStartEmail(
	stationName, eventTime string, e ChannelImbalanceData,
) (subject, body string) {
	subject = "[ALERT] Channel Imbalance Detected - " + stationName
	body = fmt.Sprintf(
		"Channel imbalance was detected at %s.\n\n"+
			"Level: Left %.1f dB / Right %.1f dB\n"+
			"Balance: %.1f dB (%s)\n"+
			"Imbalance: %.1f dB\n"+
			"Threshold: %.1f dB\n\n"+
			"The left and right channels differ by more than the configured threshold.",
		eventTime,
		e.LevelL,
		e.LevelR,
		e.BalanceDB,
		channelImbalanceDirection(e.BalanceDB),
		e.ImbalanceDB,
		e.ThresholdDB,
	)
	return subject, body
}

func buildChannelImbalanceEndEmail(
	stationName, eventTime string, e ChannelImbalanceData,
) (subject, body string) {
	subject = "[OK] Channels Balanced - " + stationName
	body = fmt.Sprintf(
		"Channels were balanced at %s.\n\n"+
			"The imbalance lasted %s.\n"+
			"Final level: Left %.1f dB / Right %.1f dB\n"+
			"Final balance: %.1f dB (%s)\n"+
			"Final imbalance: %.1f dB\n"+
			"Threshold: %.1f dB",
		eventTime,
		util.FormatDuration(e.DurationMs),
		e.LevelL,
		e.LevelR,
		e.BalanceDB,
		channelImbalanceDirection(e.BalanceDB),
		e.ImbalanceDB,
		e.ThresholdDB,
	)
	return subject, body
}

// sendDumpReadyEmailWithClient does not delegate to sendEmailWithClient because it needs to attach a file.
func sendDumpReadyEmailWithClient(
	ctx context.Context, cfg *GraphConfig, client *GraphClient, stationName string, e silenceEventData,
) error {
	subject := "[DUMP] Audio Recording - " + stationName

	body := fmt.Sprintf(
		"Audio dump ready at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		util.HumanTime(), util.FormatDuration(e.DurationMs), e.LevelL, e.LevelR, e.Threshold,
	)

	if e.Dump != nil && e.Dump.Error != nil {
		body += fmt.Sprintf("\n\nAudio recording: Failed to capture (%s)", e.Dump.Error.Error())
	}

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	// Prepare attachment if dump is available.
	var attachment *EmailAttachment
	if e.Dump != nil && e.Dump.Error == nil && e.Dump.FilePath != "" {
		data, err := os.ReadFile(e.Dump.FilePath)
		if err == nil {
			attachment = &EmailAttachment{
				Filename:    e.Dump.Filename,
				ContentType: "audio/mpeg",
				Data:        data,
			}
			body += "\n\nAudio recording attached (15s before and after the silence)."
		} else {
			slog.Warn("audio dump file unreadable, sending email without attachment",
				"path", e.Dump.FilePath, "error", err)
			body += fmt.Sprintf("\n\nAudio recording unavailable (file could not be read: %s).", err)
		}
	}

	if err := client.SendMailWithAttachment(ctx, recipients, subject, body, attachment); err != nil {
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

// IsConfiguredForImbalance reports whether the channel participates in channel imbalance flows.
func (c *EmailChannel) IsConfiguredForImbalance(cfg *config.Snapshot) bool { return cfg.HasGraph() }

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

// SubscribesChannelImbalanceStart reports whether imbalance-start events should be sent.
func (c *EmailChannel) SubscribesChannelImbalanceStart(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.ChannelImbalanceStart
}

// SubscribesChannelImbalanceEnd reports whether imbalance-end events should be sent.
func (c *EmailChannel) SubscribesChannelImbalanceEnd(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.ChannelImbalanceEnd
}

// SubscribesAudioDump reports whether audio-dump events should be sent.
func (c *EmailChannel) SubscribesAudioDump(cfg *config.Snapshot) bool {
	return cfg.HasGraph() && cfg.EmailEvents.AudioDump
}

// sendEvent builds the Graph config and cached client shared by every Send* method,
// then delegates delivery to send.
func (c *EmailChannel) sendEvent(cfg *config.Snapshot, send func(graphCfg *GraphConfig, client *GraphClient) error) error {
	graphCfg := BuildGraphConfig(cfg)
	client, err := c.getOrCreateClient(graphCfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}
	return send(graphCfg, client)
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

// SendSilenceStart sends a Microsoft Graph email for a newly detected silence event.
func (c *EmailChannel) SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		return sendSilenceEmailWithClient(ctx, graphCfg, client, cfg.StationName, silenceEventData{
			LevelL:    levelL,
			LevelR:    levelR,
			Threshold: cfg.SilenceThreshold,
		})
	})
}

// SendSilenceEnd sends a Microsoft Graph recovery email with the silence duration.
func (c *EmailChannel) SendSilenceEnd(
	ctx context.Context, cfg *config.Snapshot, durationMs int64, levelL, levelR float64,
) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		return sendSilenceEndEmailWithClient(ctx, graphCfg, client, cfg.StationName, silenceEventData{
			DurationMs: durationMs,
			LevelL:     levelL,
			LevelR:     levelR,
			Threshold:  cfg.SilenceThreshold,
		})
	})
}

// SendChannelImbalanceStart sends a Microsoft Graph email when channel imbalance is confirmed.
func (c *EmailChannel) SendChannelImbalanceStart(
	ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData,
) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		subject, body := buildChannelImbalanceStartEmail(cfg.StationName, util.HumanTime(), data)
		return sendEmailWithClient(ctx, graphCfg, client, subject, body)
	})
}

// SendChannelImbalanceEnd sends a Microsoft Graph email when stereo balance recovers.
func (c *EmailChannel) SendChannelImbalanceEnd(
	ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData,
) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		subject, body := buildChannelImbalanceEndEmail(cfg.StationName, util.HumanTime(), data)
		return sendEmailWithClient(ctx, graphCfg, client, subject, body)
	})
}

// SendAudioDump sends a Microsoft Graph email with the silence dump attached when available.
func (c *EmailChannel) SendAudioDump(
	ctx context.Context, cfg *config.Snapshot, durationMs int64, levelL, levelR float64,
	result *silencedump.EncodeResult,
) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		return sendDumpReadyEmailWithClient(ctx, graphCfg, client, cfg.StationName, silenceEventData{
			DurationMs: durationMs,
			LevelL:     levelL,
			LevelR:     levelR,
			Threshold:  cfg.SilenceThreshold,
			Dump:       result,
		})
	})
}

// SendUploadAbandoned sends a Microsoft Graph email after a recording upload exhausts retries.
func (c *EmailChannel) SendUploadAbandoned(ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData) error {
	return sendUploadAbandonedEmail(ctx, BuildGraphConfig(cfg), cfg.StationName, params)
}

// SendTestEmail validates Microsoft Graph credentials before sending a test email.
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
	if err := client.SendMail(context.Background(), recipients, subject, body); err != nil {
		return util.WrapError("send email", err)
	}

	return nil
}
