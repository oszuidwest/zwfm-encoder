// Package notify handles event notifications across multiple channels.
package notify

import (
	"log/slog"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// AlertOrchestrator manages silence and upload-abandonment alerting.
type AlertOrchestrator struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger
	dispatcher  *Dispatcher

	mu              sync.Mutex
	activeChannels  []AlertChannel
	pendingRecovery *pendingRecoveryData
}

// pendingRecoveryData holds recovery event data while waiting for the audio dump.
type pendingRecoveryData struct {
	durationMS     int64
	levelL         float64
	levelR         float64
	cfg            config.Snapshot // captured at silence-end time; reused for dump dispatch
	activeChannels []AlertChannel
}

// NewAlertOrchestrator creates a new alert orchestrator.
func NewAlertOrchestrator(cfg *config.Config, dispatcher *Dispatcher) *AlertOrchestrator {
	return &AlertOrchestrator{cfg: cfg, dispatcher: dispatcher}
}

// SetEventLogger sets the event logger for silence notifications.
func (o *AlertOrchestrator) SetEventLogger(logger *eventlog.Logger) {
	o.eventLogger = logger
}

// HandleSilenceEvent dispatches silence start and recovery notifications based on the event.
func (o *AlertOrchestrator) HandleSilenceEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		o.handleSilenceStart(event.CurrentLevelL, event.CurrentLevelR)
	}

	if event.JustRecovered {
		o.handleSilenceEnd(event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR)
	}
}

func (o *AlertOrchestrator) handleSilenceStart(levelL, levelR float64) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	if o.activeChannels == nil {
		active := make([]AlertChannel, 0, len(o.dispatcher.Channels()))
		for _, ch := range o.dispatcher.Channels() {
			if ch.IsConfiguredForSilence(&cfg) {
				active = append(active, ch)
			}
		}
		o.activeChannels = active
	}
	active := o.activeChannels
	o.mu.Unlock()

	o.dispatcher.DispatchSilenceStart(active, &cfg, levelL, levelR)
	o.logSilenceStart(&cfg, levelL, levelR)
}

func (o *AlertOrchestrator) handleSilenceEnd(durationMS int64, levelL, levelR float64) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	active := o.activeChannels
	o.activeChannels = nil
	o.pendingRecovery = &pendingRecoveryData{
		durationMS:     durationMS,
		levelL:         levelL,
		levelR:         levelR,
		cfg:            cfg,
		activeChannels: active,
	}
	o.mu.Unlock()

	o.dispatcher.DispatchSilenceEnd(active, &cfg, durationMS, levelL, levelR)
	o.logSilenceEnd(&cfg, durationMS, levelL, levelR)
}

// OnDumpReady dispatches audio_dump_ready notifications to subscribed channels.
func (o *AlertOrchestrator) OnDumpReady(result *silencedump.EncodeResult) {
	o.mu.Lock()
	pending := o.pendingRecovery
	o.pendingRecovery = nil
	o.mu.Unlock()

	if pending == nil {
		slog.Warn("audio dump ready but no pending recovery found, notifications skipped")
		return
	}

	o.dispatcher.DispatchAudioDump(pending.activeChannels, &pending.cfg, pending.durationMS, pending.levelL, pending.levelR, result)
	o.logAudioDumpReady(&pending.cfg, pending.durationMS, pending.levelL, pending.levelR, result)
}

// HandleUploadAbandoned dispatches an upload-abandonment alert to all configured channels.
func (o *AlertOrchestrator) HandleUploadAbandoned(params UploadAbandonedData) {
	cfg := o.cfg.Snapshot()
	o.dispatcher.DispatchUploadAbandoned(&cfg, params)
}

// Reset clears alert state for the current silence period, including any pending dump dispatch.
func (o *AlertOrchestrator) Reset() {
	o.mu.Lock()
	o.activeChannels = nil
	o.pendingRecovery = nil
	o.mu.Unlock()
}

// BuildGraphConfig builds a GraphConfig from a config snapshot.
func BuildGraphConfig(cfg *config.Snapshot) *GraphConfig {
	return &GraphConfig{
		TenantID:     cfg.GraphTenantID,
		ClientID:     cfg.GraphClientID,
		ClientSecret: cfg.GraphClientSecret,
		FromAddress:  cfg.GraphFromAddress,
		Recipients:   cfg.GraphRecipients,
	}
}

func (o *AlertOrchestrator) logSilenceStart(cfg *config.Snapshot, levelL, levelR float64) {
	if o.eventLogger == nil {
		return
	}
	if err := o.eventLogger.LogSilenceStart(levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence start", "error", err)
	}
}

func (o *AlertOrchestrator) logSilenceEnd(cfg *config.Snapshot, durationMS int64, levelL, levelR float64) {
	if o.eventLogger == nil {
		return
	}
	if err := o.eventLogger.LogSilenceEnd(durationMS, levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence end", "error", err)
	}
}

func (o *AlertOrchestrator) logAudioDumpReady(cfg *config.Snapshot, durationMS int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	if o.eventLogger == nil {
		return
	}

	dumpPath, dumpFilename, dumpSize, dumpError := extractDumpInfo(dump)

	if err := o.eventLogger.LogAudioDumpReady(durationMS, levelL, levelR, cfg.SilenceThreshold, dumpPath, dumpFilename, dumpSize, dumpError); err != nil {
		slog.Warn("failed to log audio dump ready", "error", err)
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
