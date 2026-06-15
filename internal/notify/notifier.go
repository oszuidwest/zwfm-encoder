// Package notify handles event notifications across multiple channels.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// logQueueDepth is the buffer size of the serialized log worker channel.
// A handful of log events are produced per audio cycle (silence start/end/dump,
// channel imbalance start/end); 16 gives ample headroom.
const logQueueDepth = 16

// logJob is a queued event-log write.
// Carrying the event type alongside the write function makes queue-full diagnostics specific
// and keeps worker behaviour explicit without relying on opaque closures.
type logJob struct {
	eventType string // event type label for diagnostics; empty for internal sentinels
	fn        func()
}

// AlertOrchestrator manages silence and upload-abandonment alerting.
type AlertOrchestrator struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger
	dispatcher  *Dispatcher

	mu              sync.Mutex
	activeChannels  []AlertChannel
	pendingRecovery *pendingRecoveryData
	notifyCtx       context.Context
	notifyCancel    context.CancelFunc

	logMu     sync.RWMutex
	logQueue  chan logJob // serialized log writes; single worker guarantees JSONL write order
	closeOnce sync.Once   // ensures Close drains and stops the worker exactly once
	closed    bool
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
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	o := &AlertOrchestrator{
		cfg:          cfg,
		dispatcher:   dispatcher,
		notifyCtx:    notifyCtx,
		notifyCancel: notifyCancel,
		logQueue:     make(chan logJob, logQueueDepth),
	}
	go o.runLogWorker()
	return o
}

// runLogWorker drains logQueue sequentially, guaranteeing that events are
// written to the JSONL file in the same order they were enqueued (i.e., event order).
func (o *AlertOrchestrator) runLogWorker() {
	for job := range o.logQueue {
		job.fn()
	}
}

// enqueueLog submits a log write for eventType to the serialized log worker.
// No-ops when no event logger is set.
// If the queue is full (logQueueDepth pending entries), the entry is dropped and a warning is emitted.
// Overflow policy: drop-and-warn. Queue saturation is only possible under extreme load
// (more than logQueueDepth events backed up behind slow file I/O), so a dropped
// entry is preferable to blocking the caller on the audio hot path.
func (o *AlertOrchestrator) enqueueLog(eventType string, fn func()) {
	if o.eventLogger == nil {
		return
	}

	o.logMu.RLock()
	defer o.logMu.RUnlock()
	if o.closed {
		return
	}

	select {
	case o.logQueue <- logJob{eventType: eventType, fn: fn}:
	default:
		slog.Warn("log queue full, log entry dropped", "event_type", eventType)
	}
}

// SetEventLogger sets the event logger for event-log writes.
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
	ctx := o.notifyCtx
	o.mu.Unlock()

	now := time.Now()
	o.dispatcher.DispatchSilenceStart(ctx, active, cfg, levelL, levelR)
	o.enqueueLog("silence_start", func() { o.logSilenceStart(now, &cfg, levelL, levelR) })
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
	ctx := o.notifyCtx
	o.mu.Unlock()

	now := time.Now()
	o.dispatcher.DispatchSilenceEnd(ctx, active, cfg, durationMS, levelL, levelR)
	o.enqueueLog("silence_end", func() { o.logSilenceEnd(now, &cfg, durationMS, levelL, levelR) })
}

// HandleChannelImbalanceEvent records channel imbalance start and end events to the
// event log. It deliberately does not touch activeChannels/pendingRecovery (which are
// owned by the silence cycle) and does not dispatch to notification channels; external
// imbalance notifications are a separate, later concern.
func (o *AlertOrchestrator) HandleChannelImbalanceEvent(event *audio.ImbalanceEvent) {
	if event.JustEntered {
		o.handleChannelImbalanceStart(event.CurrentLevelL, event.CurrentLevelR, event.BalanceDB, event.ImbalanceDB)
	}

	if event.JustRecovered {
		o.handleChannelImbalanceEnd(event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR, event.BalanceDB, event.ImbalanceDB)
	}
}

func (o *AlertOrchestrator) handleChannelImbalanceStart(levelL, levelR, balanceDB, imbalanceDB float64) {
	cfg := o.cfg.Snapshot()
	now := time.Now()
	o.enqueueLog("channel_imbalance_start", func() {
		o.logChannelImbalanceStart(now, &cfg, levelL, levelR, balanceDB, imbalanceDB)
	})
}

func (o *AlertOrchestrator) handleChannelImbalanceEnd(durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64) {
	cfg := o.cfg.Snapshot()
	now := time.Now()
	o.enqueueLog("channel_imbalance_end", func() {
		o.logChannelImbalanceEnd(now, &cfg, durationMS, levelL, levelR, balanceDB, imbalanceDB)
	})
}

// OnDumpReady dispatches audio_dump_ready notifications to subscribed channels.
func (o *AlertOrchestrator) OnDumpReady(result *silencedump.EncodeResult) {
	o.mu.Lock()
	pending := o.pendingRecovery
	o.pendingRecovery = nil
	ctx := o.notifyCtx
	o.mu.Unlock()

	if pending == nil {
		slog.Debug("audio dump ready but no pending silence recovery; dump ignored")
		return
	}

	now := time.Now()
	o.dispatcher.DispatchAudioDump(
		ctx, pending.activeChannels, pending.cfg, pending.durationMS,
		pending.levelL, pending.levelR, result,
	)
	o.enqueueLog("audio_dump_ready", func() {
		o.logAudioDumpReady(
			now, &pending.cfg, pending.durationMS,
			pending.levelL, pending.levelR, result,
		)
	})
}

// HandleUploadAbandoned dispatches an upload-abandonment alert to all configured channels.
func (o *AlertOrchestrator) HandleUploadAbandoned(params UploadAbandonedData) {
	cfg := o.cfg.Snapshot()
	o.mu.Lock()
	ctx := o.notifyCtx
	o.mu.Unlock()
	o.dispatcher.DispatchUploadAbandoned(ctx, cfg, params)
}

// Reset clears alert state for the current silence period, including any pending dump dispatch.
// In-flight notifications are intentionally NOT cancelled here: Reset runs on encoder
// start/stop/source-failure, and a short silence followed by a Stop would otherwise abort
// a still-pending silence_start delivery. The notify context is cancelled only on Close.
func (o *AlertOrchestrator) Reset() {
	o.mu.Lock()
	o.activeChannels = nil
	o.pendingRecovery = nil
	o.mu.Unlock()
}

// DrainLogs blocks until all log jobs currently in the queue have been executed.
// Safe to call multiple times; does not stop the worker. After Close, it becomes a no-op.
func (o *AlertOrchestrator) DrainLogs() {
	done := make(chan struct{})

	o.logMu.RLock()
	if o.closed {
		o.logMu.RUnlock()
		return
	}
	o.logQueue <- logJob{fn: func() { close(done) }}

	o.logMu.RUnlock()
	<-done
}

// Close drains all pending log jobs and stops the log worker.
// It also cancels in-flight notification contexts via the orchestrator's notify cancel.
// Safe to call multiple times; only the first call has effect.
// Valid for both graceful process shutdown and test cleanup.
func (o *AlertOrchestrator) Close() {
	o.closeOnce.Do(func() {
		done := make(chan struct{})

		o.mu.Lock()
		o.notifyCancel()
		o.mu.Unlock()

		o.logMu.Lock()
		o.closed = true
		queue := o.logQueue
		o.logMu.Unlock()

		queue <- logJob{fn: func() { close(done) }}
		<-done
		close(queue)
	})
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

func (o *AlertOrchestrator) logChannelImbalanceStart(t time.Time, cfg *config.Snapshot, levelL, levelR, balanceDB, imbalanceDB float64) {
	if err := o.eventLogger.LogChannelImbalanceStart(
		t, levelL, levelR, balanceDB, imbalanceDB, cfg.ChannelImbalanceThreshold,
	); err != nil {
		slog.Warn("failed to log channel imbalance start", "error", err)
	}
}

func (o *AlertOrchestrator) logChannelImbalanceEnd(t time.Time, cfg *config.Snapshot, durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64) {
	if err := o.eventLogger.LogChannelImbalanceEnd(
		t, durationMS, levelL, levelR, balanceDB, imbalanceDB, cfg.ChannelImbalanceThreshold,
	); err != nil {
		slog.Warn("failed to log channel imbalance end", "error", err)
	}
}

func (o *AlertOrchestrator) logAudioDumpReady(
	t time.Time, cfg *config.Snapshot, durationMS int64,
	levelL, levelR float64, dump *silencedump.EncodeResult,
) {
	var dumpPath, dumpFilename, dumpError string
	var dumpSize int64
	switch {
	case dump == nil:
	case dump.Error != nil:
		dumpError = dump.Error.Error()
	default:
		dumpPath, dumpFilename, dumpSize = dump.FilePath, dump.Filename, dump.FileSize
	}
	if err := o.eventLogger.LogAudioDumpReady(
		t, durationMS, levelL, levelR, cfg.SilenceThreshold,
		dumpPath, dumpFilename, dumpSize, dumpError,
	); err != nil {
		slog.Warn("failed to log audio dump ready", "error", err)
	}
}
