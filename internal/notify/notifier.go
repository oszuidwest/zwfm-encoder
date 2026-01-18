// Package notify handles silence detection notifications.
package notify

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// SilenceNotifier sends alerts when audio silence is detected and recovered.
type SilenceNotifier struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger

	// mu protects the notification state fields below
	mu sync.Mutex

	// Track which notifications have been sent for current silence period
	webhookSent bool
	emailSent   bool
	logSent     bool
	zabbixSent  bool

	// Cached Graph client for email notifications
	graphClient    *GraphClient
	graphConfigKey string // Config key used to create cached client

	// Pending recovery data (stored when recovery detected, used when dump is ready)
	pendingRecovery *pendingRecoveryData
}

// pendingRecoveryData holds recovery event data while waiting for the audio dump.
type pendingRecoveryData struct {
	durationMs int64
	levelL     float64
	levelR     float64
	cfg        config.Snapshot
	sentFlags  recoveryFlags
}

// recoveryFlags tracks which notification channels were used for silence start.
type recoveryFlags struct {
	webhook bool
	email   bool
	log     bool
	zabbix  bool
}

// NewSilenceNotifier creates a new SilenceNotifier.
func NewSilenceNotifier(cfg *config.Config) *SilenceNotifier {
	return &SilenceNotifier{cfg: cfg}
}

// SetEventLogger sets the event logger for silence notifications.
func (n *SilenceNotifier) SetEventLogger(logger *eventlog.Logger) {
	n.eventLogger = logger
}

// ResetPendingRecovery clears any pending recovery data.
func (n *SilenceNotifier) ResetPendingRecovery() {
	n.mu.Lock()
	n.pendingRecovery = nil
	n.mu.Unlock()
}

func graphConfigKeyFrom(cfg *GraphConfig) string {
	return cfg.TenantID + "|" + cfg.ClientID + "|" + cfg.ClientSecret + "|" + cfg.FromAddress
}

func (n *SilenceNotifier) getOrCreateGraphClient(cfg *GraphConfig) (*GraphClient, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	newKey := graphConfigKeyFrom(cfg)

	// Recreate client if config changed (same pattern as recorder S3 client)
	if n.graphClient != nil && n.graphConfigKey == newKey {
		return n.graphClient, nil
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return nil, err
	}
	n.graphClient = client
	n.graphConfigKey = newKey
	return client, nil
}

// HandleEvent dispatches silence start and recovery notifications based on the event.
func (n *SilenceNotifier) HandleEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		n.handleSilenceStart(event.CurrentLevelL, event.CurrentLevelR)
	}

	if event.JustRecovered {
		n.handleSilenceEnd(event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR)
	}
}

func (n *SilenceNotifier) handleSilenceStart(levelL, levelR float64) {
	cfg := n.cfg.Snapshot()

	// Determine which notifications to send (only once per silence period)
	n.mu.Lock()
	shouldSendWebhook := !n.webhookSent && cfg.HasWebhook()
	shouldSendEmail := !n.emailSent && cfg.HasGraph()
	shouldSendLog := !n.logSent && n.eventLogger != nil
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

// Reset clears notification state for the current silence period.
func (n *SilenceNotifier) Reset() {
	n.mu.Lock()
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.zabbixSent = false
	n.mu.Unlock()
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceWebhook(cfg config.Snapshot, levelL, levelR float64) {
	logNotifyResult(
		func() error { return SendWebhookSilence(cfg.WebhookURL, levelL, levelR, cfg.SilenceThreshold) },
		"Silence webhook",
	)
}

// BuildGraphConfig builds a GraphConfig from a config snapshot.
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
	logNotifyResult(
		func() error {
			return n.sendSilenceEmailWithClient(graphCfg, cfg.StationName, levelL, levelR, cfg.SilenceThreshold)
		},
		"Silence email",
	)
}

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

func (n *SilenceNotifier) sendSilenceEmailWithClient(cfg *GraphConfig, stationName string, levelL, levelR, threshold float64) error {
	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"The encoder detected silence at %s.\n\n"+
			"Audio level dropped below the %.0f dB threshold.\n"+
			"Current level: Left %.1f dB / Right %.1f dB\n\n"+
			"Silence is ongoing. Please check the audio source.",
		util.HumanTime(), threshold, levelL, levelR,
	)
	return n.sendEmail(cfg, subject, body)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceStart(cfg config.Snapshot, levelL, levelR float64) {
	if n.eventLogger == nil {
		return
	}
	if err := n.eventLogger.LogSilenceStart(levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence start", "error", err)
	}
}

// OnDumpReady completes pending recovery notifications with the audio dump attached.
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

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryWebhookWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	logNotifyResult(
		func() error {
			return sendRecoveryWebhook(cfg.WebhookURL, durationMs, levelL, levelR, cfg.SilenceThreshold, dump)
		},
		"Recovery webhook with dump",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryEmailWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	graphCfg := BuildGraphConfig(cfg)
	logNotifyResult(
		func() error {
			return n.sendRecoveryEmailWithClientAndDump(graphCfg, cfg.StationName, durationMs, levelL, levelR, cfg.SilenceThreshold, dump)
		},
		"Recovery email with dump",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceEndWithDump(cfg config.Snapshot, durationMs int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	if n.eventLogger == nil {
		return
	}

	dumpPath, dumpFilename, dumpSize, dumpError := extractDumpInfo(dump)

	if err := n.eventLogger.LogSilenceEnd(durationMs, levelL, levelR, cfg.SilenceThreshold, dumpPath, dumpFilename, dumpSize, dumpError); err != nil {
		slog.Warn("failed to log silence end", "error", err)
	}
}

func extractDumpInfo(dump *silencedump.EncodeResult) (path, filename string, size int64, errStr string) {
	if dump == nil {
		return "", "", 0, ""
	}
	if dump.Error != nil {
		return "", "", 0, dump.Error.Error()
	}
	return dump.FilePath, dump.Filename, dump.FileSize, ""
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceZabbix(cfg config.Snapshot, levelL, levelR float64) {
	logNotifyResult(
		func() error {
			return SendZabbixSilence(cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixKey, levelL, levelR, cfg.SilenceThreshold)
		},
		"Silence zabbix",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryZabbix(cfg config.Snapshot, durationMs int64, levelL, levelR float64) {
	logNotifyResult(
		func() error {
			return SendZabbixRecovery(cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixKey, durationMs, levelL, levelR, cfg.SilenceThreshold)
		},
		"Recovery zabbix",
	)
}

func sendRecoveryWebhook(webhookURL string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
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

func (n *SilenceNotifier) sendRecoveryEmailWithClientAndDump(cfg *GraphConfig, stationName string, durationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	if !IsConfigured(cfg) {
		return nil
	}

	subject := "[OK] Audio Restored - " + stationName

	// Build body with dump info
	body := fmt.Sprintf(
		"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB",
		util.HumanTime(), util.FormatDuration(durationMs), levelL, levelR,
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
