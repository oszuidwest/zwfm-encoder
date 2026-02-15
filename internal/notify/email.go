package notify

import (
	"fmt"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig is the configuration for email notifications.
type GraphConfig = types.GraphConfig

// sendUploadAbandonedEmail sends an upload abandonment alert via Microsoft Graph.
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

// SendTestEmail sends a test email to verify email configuration.
func SendTestEmail(cfg *GraphConfig, stationName string) error {
	if err := ValidateConfig(cfg); err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return fmt.Errorf("create Graph client: %w", err)
	}

	// Validate authentication first
	if err := client.ValidateAuth(); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
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
		return fmt.Errorf("send email: %w", err)
	}

	return nil
}
