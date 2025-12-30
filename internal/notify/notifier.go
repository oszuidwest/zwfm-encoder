package notify

import (
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// SilenceNotifier manages notifications for silence detection events.
type SilenceNotifier struct {
	cfg *config.Config

	// mu protects the notification state fields below
	mu sync.Mutex

	// Track which notifications have been sent for current silence period
	webhookSent bool
	emailSent   bool
	logSent     bool
}

// NewSilenceNotifier returns a SilenceNotifier configured with the given config.
func NewSilenceNotifier(cfg *config.Config) *SilenceNotifier {
	return &SilenceNotifier{cfg: cfg}
}

// HandleEvent processes a silence event and triggers notifications.
func (n *SilenceNotifier) HandleEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		n.handleSilenceStart(event.Duration)
	}

	if event.JustRecovered {
		n.handleSilenceEnd(event.TotalDuration)
	}
}

// handleSilenceStart triggers notifications when silence is first detected.
func (n *SilenceNotifier) handleSilenceStart(duration float64) {
	cfg := n.cfg.Snapshot()

	n.trySend(&n.webhookSent, cfg.HasWebhook(), func() { n.sendSilenceWebhook(cfg, duration) })
	n.trySend(&n.emailSent, cfg.HasEmail(), func() { n.sendSilenceEmail(cfg, duration) })
	n.trySend(&n.logSent, cfg.HasLogPath(), func() { n.logSilenceStart(cfg) })
}

// trySend sends a notification if the condition is met and not already sent.
func (n *SilenceNotifier) trySend(sent *bool, condition bool, sender func()) {
	n.mu.Lock()
	shouldSend := !*sent && condition
	if shouldSend {
		*sent = true
	}
	n.mu.Unlock()
	if shouldSend {
		go sender()
	}
}

// handleSilenceEnd triggers recovery notifications when silence ends.
func (n *SilenceNotifier) handleSilenceEnd(totalDuration float64) {
	cfg := n.cfg.Snapshot()

	// Only send recovery notifications if we sent the corresponding start notification
	n.mu.Lock()
	shouldSendWebhookRecovery := n.webhookSent
	shouldSendEmailRecovery := n.emailSent
	shouldSendLogRecovery := n.logSent
	// Reset notification state for next silence period
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.mu.Unlock()

	if shouldSendWebhookRecovery {
		go n.sendRecoveryWebhook(cfg, totalDuration)
	}

	if shouldSendEmailRecovery {
		go n.sendRecoveryEmail(cfg, totalDuration)
	}

	if shouldSendLogRecovery {
		go n.logSilenceEnd(cfg, totalDuration)
	}
}

// Reset clears the notification state.
func (n *SilenceNotifier) Reset() {
	n.mu.Lock()
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.mu.Unlock()
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceWebhook(cfg config.Snapshot, duration float64) {
	util.LogNotifyResult(
		func() error { return SendSilenceWebhook(cfg.WebhookURL, duration, cfg.SilenceThreshold) },
		"Silence webhook",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryWebhook(cfg config.Snapshot, duration float64) {
	util.LogNotifyResult(
		func() error { return SendRecoveryWebhook(cfg.WebhookURL, duration) },
		"Recovery webhook",
	)
}

// BuildEmailConfig creates an EmailConfig from the config snapshot.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func BuildEmailConfig(cfg config.Snapshot) *EmailConfig {
	return &EmailConfig{
		Host:       cfg.EmailSMTPHost,
		Port:       cfg.EmailSMTPPort,
		FromName:   cfg.EmailFromName,
		Username:   cfg.EmailUsername,
		Password:   cfg.EmailPassword,
		Recipients: cfg.EmailRecipients,
	}
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceEmail(cfg config.Snapshot, duration float64) {
	emailCfg := BuildEmailConfig(cfg)
	util.LogNotifyResult(
		func() error { return SendSilenceAlert(emailCfg, cfg.StationName, duration, cfg.SilenceThreshold) },
		"Silence email",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryEmail(cfg config.Snapshot, duration float64) {
	emailCfg := BuildEmailConfig(cfg)
	util.LogNotifyResult(
		func() error { return SendRecoveryAlert(emailCfg, cfg.StationName, duration) },
		"Recovery email",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceStart(cfg config.Snapshot) {
	util.LogNotifyResult(
		func() error { return LogSilenceStart(cfg.LogPath, cfg.SilenceThreshold) },
		"Silence log",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceEnd(cfg config.Snapshot, duration float64) {
	util.LogNotifyResult(
		func() error { return LogSilenceEnd(cfg.LogPath, duration, cfg.SilenceThreshold) },
		"Recovery log",
	)
}
