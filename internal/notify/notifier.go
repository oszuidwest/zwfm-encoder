package notify

import (
	"fmt"
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

	// Cached Graph client for email notifications
	graphClient *GraphClient
}

// NewSilenceNotifier returns a SilenceNotifier configured with the given config.
func NewSilenceNotifier(cfg *config.Config) *SilenceNotifier {
	return &SilenceNotifier{cfg: cfg}
}

// InvalidateGraphClient clears the cached Graph client.
// Call this when Graph configuration changes.
func (n *SilenceNotifier) InvalidateGraphClient() {
	n.mu.Lock()
	n.graphClient = nil
	n.mu.Unlock()
}

// getOrCreateGraphClient returns the cached Graph client, creating it if needed.
func (n *SilenceNotifier) getOrCreateGraphClient(cfg *GraphConfig) (*GraphClient, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.graphClient != nil {
		return n.graphClient, nil
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return nil, err
	}
	n.graphClient = client
	return client, nil
}

// HandleEvent processes a silence event and triggers notifications.
func (n *SilenceNotifier) HandleEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		n.handleSilenceStart(event.CurrentLevelL, event.CurrentLevelR)
	}

	if event.JustRecovered {
		n.handleSilenceEnd(event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR)
	}
}

// handleSilenceStart triggers notifications when silence is first detected.
func (n *SilenceNotifier) handleSilenceStart(levelL, levelR float64) {
	cfg := n.cfg.Snapshot()

	n.trySend(&n.webhookSent, cfg.HasWebhook(), func() { n.sendSilenceWebhook(cfg, levelL, levelR) })
	n.trySend(&n.emailSent, cfg.HasGraph(), func() { n.sendSilenceEmail(cfg, levelL, levelR) })
	n.trySend(&n.logSent, cfg.HasLogPath(), func() { n.logSilenceStart(cfg, levelL, levelR) })
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
func (n *SilenceNotifier) handleSilenceEnd(totalDurationMs int64, levelL, levelR float64) {
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
		go n.sendRecoveryWebhook(cfg, totalDurationMs, levelL, levelR)
	}

	if shouldSendEmailRecovery {
		go n.sendRecoveryEmail(cfg, totalDurationMs, levelL, levelR)
	}

	if shouldSendLogRecovery {
		go n.logSilenceEnd(cfg, totalDurationMs, levelL, levelR)
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
func (n *SilenceNotifier) sendSilenceWebhook(cfg config.Snapshot, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return SendSilenceWebhook(cfg.WebhookURL, levelL, levelR, cfg.SilenceThreshold) },
		"Silence webhook",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryWebhook(cfg config.Snapshot, durationMs int64, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return SendRecoveryWebhook(cfg.WebhookURL, durationMs, levelL, levelR, cfg.SilenceThreshold) },
		"Recovery webhook",
	)
}

// BuildGraphConfig creates a GraphConfig from the config snapshot.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func BuildGraphConfig(cfg config.Snapshot) *GraphConfig {
	return &GraphConfig{
		TenantID:     cfg.GraphTenantID,
		ClientID:     cfg.GraphClientID,
		ClientSecret: cfg.GraphClientSecret,
		FromAddress:  cfg.GraphFromAddress,
		Recipients:   cfg.GraphRecipients,
	}
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceEmail(cfg config.Snapshot, levelL, levelR float64) {
	graphCfg := BuildGraphConfig(cfg)
	util.LogNotifyResult(
		func() error {
			return n.sendSilenceEmailWithClient(graphCfg, cfg.StationName, levelL, levelR, cfg.SilenceThreshold)
		},
		"Silence email",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryEmail(cfg config.Snapshot, durationMs int64, levelL, levelR float64) {
	graphCfg := BuildGraphConfig(cfg)
	util.LogNotifyResult(
		func() error {
			return n.sendRecoveryEmailWithClient(graphCfg, cfg.StationName, durationMs, levelL, levelR, cfg.SilenceThreshold)
		},
		"Recovery email",
	)
}

// sendEmail handles the common email sending infrastructure.
func (n *SilenceNotifier) sendEmail(cfg *GraphConfig, subject, body string) error {
	if !IsConfigured(cfg) {
		return nil
	}

	client, err := n.getOrCreateGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	if err := client.SendMail(recipients, subject, body); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}

// sendSilenceEmailWithClient sends a silence alert email using the cached Graph client.
func (n *SilenceNotifier) sendSilenceEmailWithClient(cfg *GraphConfig, stationName string, levelL, levelR, threshold float64) error {
	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"Silence detected on the audio encoder.\n\n"+
			"Level:     L %.1f dB / R %.1f dB\n"+
			"Threshold: %.1f dB\n"+
			"Time:      %s\n\n"+
			"Silence is ongoing. Please check the audio source.",
		levelL, levelR, threshold, util.HumanTime(),
	)
	return n.sendEmail(cfg, subject, body)
}

// sendRecoveryEmailWithClient sends a recovery email using the cached Graph client.
func (n *SilenceNotifier) sendRecoveryEmailWithClient(cfg *GraphConfig, stationName string, durationMs int64, levelL, levelR, threshold float64) error {
	subject := "[OK] Audio Recovered - " + stationName
	body := fmt.Sprintf(
		"Audio recovered on the encoder.\n\n"+
			"Level:          L %.1f dB / R %.1f dB\n"+
			"Silence lasted: %s\n"+
			"Threshold:      %.1f dB\n"+
			"Time:           %s",
		levelL, levelR, util.FormatDuration(durationMs), threshold, util.HumanTime(),
	)
	return n.sendEmail(cfg, subject, body)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceStart(cfg config.Snapshot, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return LogSilenceStart(cfg.LogPath, levelL, levelR, cfg.SilenceThreshold) },
		"Silence log",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceEnd(cfg config.Snapshot, durationMs int64, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return LogSilenceEnd(cfg.LogPath, durationMs, levelL, levelR, cfg.SilenceThreshold) },
		"Recovery log",
	)
}
