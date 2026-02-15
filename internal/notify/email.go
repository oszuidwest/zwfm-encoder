package notify

import (
	"fmt"
	"os"

	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig is the configuration for email notifications.
type GraphConfig = types.GraphConfig

// sendUploadAbandonedEmail sends an upload abandonment alert via Microsoft Graph.
// A fresh client is created each time because upload abandonments are rare one-off events
// where caching would add complexity without meaningful benefit.
func sendUploadAbandonedEmail(cfg *GraphConfig, stationName string, p UploadAbandonedParams) error {
	if !IsConfigured(cfg) {
		return nil
	}

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

func (n *SilenceNotifier) sendRecoveryEmailWithClientAndDump(cfg *GraphConfig, stationName string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	if !IsConfigured(cfg) {
		return nil
	}

	subject := "[OK] Audio Restored - " + stationName

	// Build body with dump info
	body := fmt.Sprintf(
		"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		util.HumanTime(), util.FormatDuration(durationMs), levelL, levelR, threshold,
	)

	// Add dump info to body
	if dump != nil {
		if dump.Error != nil {
			body += fmt.Sprintf("\n\nAudio recording: Failed to capture (%s)", dump.Error.Error())
		} else {
			body += "\n\nAudio recording attached (15s before and after the silence)."
		}
	}

	client, err := n.getOrCreateGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	// Prepare attachment if dump is available
	var attachment *EmailAttachment
	if dump != nil && dump.Error == nil && dump.FilePath != "" {
		data, err := os.ReadFile(dump.FilePath)
		if err == nil {
			attachment = &EmailAttachment{
				Filename:    dump.Filename,
				ContentType: "audio/mpeg",
				Data:        data,
			}
		}
	}

	if err := client.SendMailWithAttachment(recipients, subject, body, attachment); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
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
