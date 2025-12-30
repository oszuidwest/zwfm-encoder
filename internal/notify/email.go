// Package notify provides notification services for silence alerts.
package notify

import (
	"fmt"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"github.com/wneessen/go-mail"
)

// EmailConfig is the SMTP configuration for email notifications.
type EmailConfig = types.EmailConfig

// SendSilenceAlert sends an email notification for critical silence.
func SendSilenceAlert(cfg *EmailConfig, stationName string, duration, threshold float64) error {
	if !util.IsConfigured(cfg.Host, cfg.Username, cfg.Recipients) {
		return nil // Silently skip if not configured
	}

	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"Silence detected on the audio encoder.\n\n"+
			"Duration:  %.1f seconds\n"+
			"Threshold: %.1f dB\n"+
			"Time:      %s\n\n"+
			"Please check the audio source.",
		duration, threshold, util.HumanTime(),
	)

	return sendEmail(cfg, subject, body)
}

// SendRecoveryAlert sends an email notification when audio recovers from silence.
func SendRecoveryAlert(cfg *EmailConfig, stationName string, silenceDuration float64) error {
	if !util.IsConfigured(cfg.Host, cfg.Username, cfg.Recipients) {
		return nil // Silently skip if not configured
	}

	subject := "[OK] Audio Recovered - " + stationName
	body := fmt.Sprintf(
		"Audio recovered on the encoder.\n\n"+
			"Silence lasted: %.1f seconds\n"+
			"Time:           %s",
		silenceDuration, util.HumanTime(),
	)

	return sendEmail(cfg, subject, body)
}

// SendTestEmail sends a test email to verify SMTP configuration.
func SendTestEmail(cfg *EmailConfig, stationName string) error {
	if cfg.Host == "" {
		return fmt.Errorf("SMTP host not configured")
	}
	if cfg.Username == "" {
		return fmt.Errorf("email username not configured")
	}
	if cfg.Recipients == "" {
		return fmt.Errorf("email recipients not configured")
	}

	subject := "[TEST] " + stationName
	body := fmt.Sprintf(
		"Test email from the audio encoder.\n\n"+
			"Time: %s\n\n"+
			"SMTP configuration is working correctly.",
		util.HumanTime(),
	)

	return sendEmail(cfg, subject, body)
}

// sendEmail delivers an email message to configured recipients.
func sendEmail(cfg *EmailConfig, subject, body string) error {
	var recipients []string
	for _, r := range strings.Split(cfg.Recipients, ",") {
		if r = strings.TrimSpace(r); r != "" {
			recipients = append(recipients, r)
		}
	}
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	m := mail.NewMsg()
	if cfg.FromName != "" {
		if err := m.FromFormat(cfg.FromName, cfg.Username); err != nil {
			return util.WrapError("set from address", err)
		}
	} else {
		if err := m.From(cfg.Username); err != nil {
			return util.WrapError("set from address", err)
		}
	}
	if err := m.To(recipients...); err != nil {
		return util.WrapError("set recipient address", err)
	}
	m.Subject(subject)
	m.SetBodyString(mail.TypeTextPlain, body)

	// Build client options with port-appropriate TLS settings
	opts := []mail.Option{
		mail.WithPort(cfg.Port),
		mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover),
		mail.WithUsername(cfg.Username),
		mail.WithPassword(cfg.Password),
	}

	switch cfg.Port {
	case 465: // SMTPS - implicit TLS
		opts = append(opts, mail.WithSSL())
	case 587: // Submission - STARTTLS required
		opts = append(opts, mail.WithTLSPortPolicy(mail.TLSMandatory))
	default: // Port 25 or custom - opportunistic TLS
		opts = append(opts, mail.WithTLSPortPolicy(mail.TLSOpportunistic))
	}

	c, err := mail.NewClient(cfg.Host, opts...)
	if err != nil {
		return util.WrapError("create SMTP client", err)
	}

	if err := c.DialAndSend(m); err != nil {
		return util.WrapError("send email", err)
	}

	return nil
}
