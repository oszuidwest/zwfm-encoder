// Package notify handles event notifications across multiple channels.
package notify

import (
	"log/slog"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// logQueueDepth is the buffer size of the serialized log worker channel.
// Three log events are produced per silence cycle (start, end, dump); 16 gives ample headroom.
const logQueueDepth = 16

// AlertOrchestrator manages silence and upload-abandonment alerting.
type AlertOrchestrator struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger
	dispatcher  *Dispatcher

	mu              sync.Mutex
	activeChannels  []AlertChannel
	pendingRecovery *pendingRecoveryData

	logQueue chan func() // serialized log writes; single worker guarantees JSONL write order
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
	o := &AlertOrchestrator{
		cfg:        cfg,
		dispatcher: dispatcher,
		logQueue:   make(chan func(), logQueueDepth),
	}
	go o.runLogWorker()
	return o
}

// runLogWorker drains logQueue sequentially, guaranteeing that silence events are
// written to the JSONL file in the same order they were enqueued (i.e., event order).
func (o *AlertOrchestrator) runLogWorker() {
	for fn := range o.logQueue {
		fn()
	}
}

// enqueueLog submits fn to the serialized log worker. No-ops when no event logger is set.
// If the queue is full (logQueueDepth pending entries), the entry is dropped and a warning is emitted.
func (o *AlertOrchestrator) enqueueLog(fn func()) {
	if o.eventLogger == nil {
		return
	}
	select {
	case o.logQueue <- fn:
	default:
		slog.Warn("silence log queue full, log entry dropped")
	}
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

	now := time.Now()
	o.dispatcher.DispatchSilenceStart(active, cfg, levelL, levelR)
	o.enqueueLog(func() { o.logSilenceStart(now, &cfg, levelL, levelR) })
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

	now := time.Now()
	o.dispatcher.DispatchSilenceEnd(active, cfg, durationMS, levelL, levelR)
	o.enqueueLog(func() { o.logSilenceEnd(now, &cfg, durationMS, levelL, levelR) })
}

// OnDumpReady dispatches audio_dump_ready notifications to subscribed channels.
func (o *AlertOrchestrator) OnDumpReady(result *silencedump.EncodeResult) {
	o.mu.Lock()
	pending := o.pendingRecovery
	o.pendingRecovery = nil
	o.mu.Unlock()

	if pending == nil {
		slog.Debug("audio dump ready but no pending silence recovery; dump ignored")
		return
	}

	now := time.Now()
	o.dispatcher.DispatchAudioDump(pending.activeChannels, pending.cfg, pending.durationMS, pending.levelL, pending.levelR, result)
	o.enqueueLog(func() {
		o.logAudioDumpReady(now, &pending.cfg, pending.durationMS, pending.levelL, pending.levelR, result)
	})
}

// HandleUploadAbandoned dispatches an upload-abandonment alert to all configured channels.
func (o *AlertOrchestrator) HandleUploadAbandoned(params UploadAbandonedData) {
	cfg := o.cfg.Snapshot()
	o.dispatcher.DispatchUploadAbandoned(cfg, params)
}

// Reset clears alert state for the current silence period, including any pending dump dispatch.
func (o *AlertOrchestrator) Reset() {
	o.mu.Lock()
	o.activeChannels = nil
	o.pendingRecovery = nil
	o.mu.Unlock()
}

// DrainLogs blocks until all log jobs currently in the queue have been executed.
// Safe to call multiple times (e.g. on Stop and again after a restart cycle).
func (o *AlertOrchestrator) DrainLogs() {
	done := make(chan struct{})
	o.logQueue <- func() { close(done) }
	<-done
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

func (o *AlertOrchestrator) logSilenceStart(t time.Time, cfg *config.Snapshot, levelL, levelR float64) {
	if err := o.eventLogger.LogSilenceStart(t, levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence start", "error", err)
	}
}

func (o *AlertOrchestrator) logSilenceEnd(t time.Time, cfg *config.Snapshot, durationMS int64, levelL, levelR float64) {
	if err := o.eventLogger.LogSilenceEnd(t, durationMS, levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence end", "error", err)
	}
}

func (o *AlertOrchestrator) logAudioDumpReady(t time.Time, cfg *config.Snapshot, durationMS int64, levelL, levelR float64, dump *silencedump.EncodeResult) {
	dumpPath, dumpFilename, dumpSize, dumpError := extractDumpInfo(dump)
	if err := o.eventLogger.LogAudioDumpReady(t, durationMS, levelL, levelR, cfg.SilenceThreshold, dumpPath, dumpFilename, dumpSize, dumpError); err != nil {
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
