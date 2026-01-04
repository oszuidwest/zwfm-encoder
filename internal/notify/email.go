// Package notify provides notification services for silence alerts.
package notify

import (
	"fmt"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig is the configuration for email notifications.
type GraphConfig = types.GraphConfig

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
