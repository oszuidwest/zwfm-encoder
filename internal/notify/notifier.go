package notify

import (
	"encoding/base64"
	"fmt"
	"os"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// SilenceNotifier manages notifications for silence detection events.
// Audio dump capture is handled by silencedump.Manager; this notifier
// receives dump results via OnDumpReady callback.
type SilenceNotifier struct {
	cfg *config.Config

	// mu protects the notification state fields below
	mu sync.Mutex

	// Track which notifications have been sent for current silence period
	webhookSent bool
	emailSent   bool
	logSent     bool
	zabbixSent  bool

	// Cached Graph client for email notifications
	graphClient *GraphClient

	// Pending recovery data (stored when recovery detected, used when dump is ready)
	pendingRecovery *pendingRecoveryData
}

// pendingRecoveryData stores recovery event data while waiting for dump.
type pendingRecoveryData struct {
	durationMs int64
	levelL     float64
	levelR     float64
	cfg        config.Snapshot
	sentFlags  recoveryFlags
}

// recoveryFlags tracks which recovery notifications should be sent.
type recoveryFlags struct {
	webhook bool
	email   bool
	log     bool
	zabbix  bool
}

// NewSilenceNotifier returns a SilenceNotifier configured with the given config.
func NewSilenceNotifier(cfg *config.Config) *SilenceNotifier {
	return &SilenceNotifier{cfg: cfg}
}

// ResetPendingRecovery clears any pending recovery notification state.
func (n *SilenceNotifier) ResetPendingRecovery() {
	n.mu.Lock()
	n.pendingRecovery = nil
	n.mu.Unlock()
}

// InvalidateGraphClient clears the cached Graph client.
func (n *SilenceNotifier) InvalidateGraphClient() {
	n.mu.Lock()
	n.graphClient = nil
	n.mu.Unlock()
}

// getOrCreateGraphClient returns the Graph client.
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
// Note: Silence dump capture is handled separately by silencedump.Manager.
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

	// Determine which notifications to send (only once per silence period)
	n.mu.Lock()
	shouldSendWebhook := !n.webhookSent && cfg.HasWebhook()
	shouldSendEmail := !n.emailSent && cfg.HasGraph()
	shouldSendLog := !n.logSent && cfg.HasLogPath()
	shouldSendZabbix := !n.zabbixSent && cfg.HasZabbix()
	if shouldSendWebhook {
		n.webhookSent = true
	}
	if shouldSendEmail {
		n.emailSent = true
	}
	if shouldSendLog {
		n.logSent = true
	}
	if shouldSendZabbix {
		n.zabbixSent = true
	}
	n.mu.Unlock()

	if shouldSendWebhook {
		go n.sendSilenceWebhook(cfg, levelL, levelR)
	}
	if shouldSendEmail {
		go n.sendSilenceEmail(cfg, levelL, levelR)
	}
	if shouldSendLog {
		go n.logSilenceStart(cfg, levelL, levelR)
	}
	if shouldSendZabbix {
		go n.sendSilenceZabbix(cfg, levelL, levelR)
	}
}

// handleSilenceEnd stores recovery data for later notification with audio dump.
func (n *SilenceNotifier) handleSilenceEnd(totalDurationMs int64, levelL, levelR float64) {
	cfg := n.cfg.Snapshot()

	// Only send recovery notifications if we sent the corresponding start notification
	n.mu.Lock()
	shouldSendWebhookRecovery := n.webhookSent
	shouldSendEmailRecovery := n.emailSent
	shouldSendLogRecovery := n.logSent
	shouldSendZabbixRecovery := n.zabbixSent
	// Reset notification state for next silence period
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.zabbixSent = false

	// Store pending recovery data for when clip is ready
	n.pendingRecovery = &pendingRecoveryData{
		durationMs: totalDurationMs,
		levelL:     levelL,
		levelR:     levelR,
		cfg:        cfg,
		sentFlags: recoveryFlags{
			webhook: shouldSendWebhookRecovery,
			email:   shouldSendEmailRecovery,
			log:     shouldSendLogRecovery,
			zabbix:  shouldSendZabbixRecovery,
		},
	}
	n.mu.Unlock()
}

// Reset clears the notification state.
func (n *SilenceNotifier) Reset() {
	n.mu.Lock()
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.zabbixSent = false
	n.mu.Unlock()
}

// sendSilenceWebhook sends a silence detection webhook notification.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceWebhook(cfg config.Snapshot, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return SendSilenceWebhook(cfg.WebhookURL, levelL, levelR, cfg.SilenceThreshold) },
		"Silence webhook",
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

// sendSilenceEmail sends a silence detection email notification.
//
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

// sendEmail sends an email using the given configuration.
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

// sendSilenceEmailWithClient sends a silence alert email.
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

// logSilenceStart logs the start of a silence event.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceStart(cfg config.Snapshot, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error { return LogSilenceStart(cfg.LogPath, levelL, levelR, cfg.SilenceThreshold) },
		"Silence log",
	)
}

// OnDumpReady sends recovery notifications with the audio dump attached.
func (n *SilenceNotifier) OnDumpReady(result *silencedump.EncodeResult) {
	n.mu.Lock()
	pending := n.pendingRecovery
	n.pendingRecovery = nil
	n.mu.Unlock()

	if pending == nil {
		return
	}

	// Send recovery notifications with dump
	if pending.sentFlags.webhook {
		go n.sendRecoveryWebhookWithDump(pending.cfg, pending.durationMs, pending.levelL, pending.levelR, result)
	}
	if pending.sentFlags.email {
		go n.sendRecoveryEmailWithDump(pending.cfg, pending.durationMs, pending.levelL, pending.levelR, result)
	}
	if pending.sentFlags.log {
		go n.logSilenceEndWithDump(pending.cfg, pending.durationMs, pending.levelL, pending.levelR, result)
	}
	if pending.sentFlags.zabbix {
		go n.sendRecoveryZabbix(pending.cfg, pending.durationMs, pending.levelL, pending.levelR)
	}
}

// sendRecoveryWebhookWithDump sends a recovery webhook with audio dump.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryWebhookWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	util.LogNotifyResult(
		func() error {
			return sendRecoveryWebhookWithDump(cfg.WebhookURL, durationMs, levelL, levelR, cfg.SilenceThreshold, dump)
		},
		"Recovery webhook with dump",
	)
}

// sendRecoveryEmailWithDump sends a recovery email with audio dump attachment.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryEmailWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	graphCfg := BuildGraphConfig(cfg)
	util.LogNotifyResult(
		func() error {
			return n.sendRecoveryEmailWithClientAndDump(graphCfg, cfg.StationName, durationMs, levelL, levelR, cfg.SilenceThreshold, dump)
		},
		"Recovery email with dump",
	)
}

// logSilenceEndWithDump logs the end of a silence event with dump info.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceEndWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	util.LogNotifyResult(
		func() error {
			return LogSilenceEndWithDump(cfg.LogPath, durationMs, levelL, levelR, cfg.SilenceThreshold, dump)
		},
		"Recovery log with dump",
	)
}

// sendSilenceZabbix sends a silence alert to Zabbix.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceZabbix(cfg config.Snapshot, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error {
			return SendSilenceZabbix(cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixKey, levelL, levelR, cfg.SilenceThreshold, cfg.ZabbixTimeoutMs)
		},
		"Silence zabbix",
	)
}

// sendRecoveryZabbix sends a recovery message to Zabbix.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryZabbix(cfg config.Snapshot, durationMs int64, levelL, levelR float64) {
	util.LogNotifyResult(
		func() error {
			return SendRecoveryZabbix(cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixKey, durationMs, levelL, levelR, cfg.SilenceThreshold, cfg.ZabbixTimeoutMs)
		},
		"Recovery zabbix",
	)
}

// sendRecoveryWebhookWithDump sends a recovery webhook with optional audio dump.
func sendRecoveryWebhookWithDump(webhookURL string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	payload := &WebhookPayload{
		Event:             "silence_recovered",
		SilenceDurationMs: durationMs,
		LevelLeftDB:       levelL,
		LevelRightDB:      levelR,
		Threshold:         threshold,
		Timestamp:         timestampUTC(),
	}

	// Add dump info
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

// sendRecoveryEmailWithClientAndDump sends a recovery email with optional audio dump attachment.
func (n *SilenceNotifier) sendRecoveryEmailWithClientAndDump(cfg *GraphConfig, stationName string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	if !IsConfigured(cfg) {
		return nil
	}

	subject := "[OK] Audio Recovered - " + stationName

	// Build body with dump info
	body := fmt.Sprintf(
		"Audio recovered on the encoder.\n\n"+
			"Level:          L %.1f dB / R %.1f dB\n"+
			"Silence lasted: %s\n"+
			"Threshold:      %.1f dB\n"+
			"Time:           %s",
		levelL, levelR, util.FormatDuration(durationMs), threshold, util.HumanTime(),
	)

	// Add dump info to body
	if dump != nil {
		if dump.Error != nil {
			body += fmt.Sprintf("\n\nAudio dump: Failed to capture (%s)", dump.Error.Error())
		} else {
			body += fmt.Sprintf("\n\nAudio dump attached: %s (15s before, silence, 15s after)", dump.Filename)
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
