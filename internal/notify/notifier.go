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

// logQueueDepth buffers ordered event-log writes from audio cycle handlers.
const logQueueDepth = 16

// pendingRecoveryTTL exceeds the 15-second post window plus the 30-second
// encoder timeout, and bounds stale entries when capture is disabled.
const pendingRecoveryTTL = time.Minute

// logJob is a queued event-log write.
// eventType keeps queue-full diagnostics specific without inspecting the closure.
type logJob struct {
	eventType string // event type label for diagnostics; empty for internal sentinels
	fn        func()
}

// AlertOrchestrator coordinates silence, channel-imbalance, audio-dump, and
// upload-abandonment alert lifecycles.
type AlertOrchestrator struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger
	dispatcher  *Dispatcher

	mu                      sync.Mutex
	activeChannels          []AlertChannel
	imbalanceActiveChannels []AlertChannel
	pendingRecoveries       map[incidentKey]*pendingRecoveryData
	notifyCtx               context.Context
	notifyCancel            context.CancelFunc

	logMu     sync.RWMutex
	logQueue  chan logJob // serialized log writes; single worker guarantees JSONL write order
	closeOnce sync.Once   // ensures Close drains and stops the worker exactly once
	closed    bool
}

// pendingRecoveryData holds recovery event data while waiting for the audio dump.
type pendingRecoveryData struct {
	dumpData       AudioDumpData
	cfg            config.Snapshot // captured at recovery time; reused for dump dispatch
	activeChannels []AlertChannel
	recoveredAt    time.Time
}

type incidentKey struct {
	trigger    silencedump.Trigger
	incidentID audio.IncidentID
}

// NewAlertOrchestrator wires notification dispatch and starts the ordered event-log worker.
func NewAlertOrchestrator(cfg *config.Config, dispatcher *Dispatcher) *AlertOrchestrator {
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	o := &AlertOrchestrator{
		cfg:               cfg,
		dispatcher:        dispatcher,
		notifyCtx:         notifyCtx,
		notifyCancel:      notifyCancel,
		logQueue:          make(chan logJob, logQueueDepth),
		pendingRecoveries: map[incidentKey]*pendingRecoveryData{},
	}
	go o.runLogWorker()
	return o
}

// runLogWorker writes queued log jobs in enqueue order.
func (o *AlertOrchestrator) runLogWorker() {
	for job := range o.logQueue {
		job.fn()
	}
}

// enqueueLog sends a log write to the ordered worker.
// It drops and warns instead of blocking the audio path when the queue is full.
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

// HandleSilenceEvent translates detector transitions into silence notification lifecycles.
func (o *AlertOrchestrator) HandleSilenceEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		o.handleSilenceStart(event.IncidentID, event.CurrentLevelL, event.CurrentLevelR)
	}

	if event.JustRecovered {
		o.handleSilenceEnd(event.IncidentID, event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR)
	}
}

func (o *AlertOrchestrator) handleSilenceStart(incidentID audio.IncidentID, levelL, levelR float64) {
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
	o.enqueueLog("silence_start", func() { o.logSilenceStart(now, &cfg, incidentID, levelL, levelR) })
}

func (o *AlertOrchestrator) handleSilenceEnd(
	incidentID audio.IncidentID, durationMS int64, levelL, levelR float64,
) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	active := o.activeChannels
	o.activeChannels = nil
	now := time.Now()
	o.addPendingRecoveryLocked(&pendingRecoveryData{
		dumpData: AudioDumpData{
			Trigger:     silencedump.TriggerSilence,
			IncidentID:  incidentID,
			DurationMs:  durationMS,
			LevelL:      levelL,
			LevelR:      levelR,
			ThresholdDB: cfg.SilenceThreshold,
		},
		cfg:            cfg,
		activeChannels: active,
		recoveredAt:    now,
	})
	ctx := o.notifyCtx
	o.mu.Unlock()

	o.dispatcher.DispatchSilenceEnd(ctx, active, cfg, durationMS, levelL, levelR)
	o.enqueueLog("silence_end", func() { o.logSilenceEnd(now, &cfg, incidentID, durationMS, levelL, levelR) })
}

// HandleChannelImbalanceEvent translates detector transitions into imbalance notifications.
// It leaves silence notification state untouched.
func (o *AlertOrchestrator) HandleChannelImbalanceEvent(event *audio.ImbalanceEvent) {
	if event.JustEntered {
		o.handleChannelImbalanceStart(
			event.IncidentID, event.CurrentLevelL, event.CurrentLevelR, event.BalanceDB, event.ImbalanceDB,
		)
	}

	if event.JustRecovered {
		o.handleChannelImbalanceEnd(
			event.IncidentID, event.TotalDurationMs, event.CurrentLevelL, event.CurrentLevelR,
			event.BalanceDB, event.ImbalanceDB,
		)
	}
}

func (o *AlertOrchestrator) handleChannelImbalanceStart(
	incidentID audio.IncidentID, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	if o.imbalanceActiveChannels == nil {
		active := make([]AlertChannel, 0, len(o.dispatcher.Channels()))
		for _, ch := range o.dispatcher.Channels() {
			if ch.IsConfiguredForImbalance(&cfg) {
				active = append(active, ch)
			}
		}
		o.imbalanceActiveChannels = active
	}
	active := o.imbalanceActiveChannels
	ctx := o.notifyCtx
	o.mu.Unlock()

	data := ChannelImbalanceData{
		LevelL:      levelL,
		LevelR:      levelR,
		BalanceDB:   balanceDB,
		ImbalanceDB: imbalanceDB,
		ThresholdDB: cfg.ChannelImbalanceThreshold,
	}
	now := time.Now()
	o.dispatcher.DispatchChannelImbalanceStart(ctx, active, cfg, data)
	o.enqueueLog("channel_imbalance_start", func() {
		o.logChannelImbalanceStart(now, &cfg, incidentID, levelL, levelR, balanceDB, imbalanceDB)
	})
}

func (o *AlertOrchestrator) handleChannelImbalanceEnd(
	incidentID audio.IncidentID, durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	active := o.imbalanceActiveChannels
	o.imbalanceActiveChannels = nil
	now := time.Now()
	o.addPendingRecoveryLocked(&pendingRecoveryData{
		dumpData: AudioDumpData{
			Trigger:     silencedump.TriggerChannelImbalance,
			IncidentID:  incidentID,
			DurationMs:  durationMS,
			LevelL:      levelL,
			LevelR:      levelR,
			BalanceDB:   &balanceDB,
			ImbalanceDB: &imbalanceDB,
			ThresholdDB: cfg.ChannelImbalanceThreshold,
		},
		cfg:            cfg,
		activeChannels: active,
		recoveredAt:    now,
	})
	ctx := o.notifyCtx
	o.mu.Unlock()

	data := ChannelImbalanceData{
		LevelL:      levelL,
		LevelR:      levelR,
		BalanceDB:   balanceDB,
		ImbalanceDB: imbalanceDB,
		ThresholdDB: cfg.ChannelImbalanceThreshold,
		DurationMs:  durationMS,
	}
	o.dispatcher.DispatchChannelImbalanceEnd(ctx, active, cfg, data)
	o.enqueueLog("channel_imbalance_end", func() {
		o.logChannelImbalanceEnd(now, &cfg, incidentID, durationMS, levelL, levelR, balanceDB, imbalanceDB)
	})
}

// addPendingRecoveryLocked records a recovery under its incident identity and
// removes callbacks that can no longer arrive within the capture and encoder
// deadlines. The caller holds o.mu.
func (o *AlertOrchestrator) addPendingRecoveryLocked(pending *pendingRecoveryData) {
	cutoff := pending.recoveredAt.Add(-pendingRecoveryTTL)
	for existingKey, existing := range o.pendingRecoveries {
		if existing.recoveredAt.Before(cutoff) {
			delete(o.pendingRecoveries, existingKey)
		}
	}
	key := incidentKey{trigger: pending.dumpData.Trigger, incidentID: pending.dumpData.IncidentID}
	o.pendingRecoveries[key] = pending
}

// OnDumpReady completes the recovery matching an encoded audio-incident dump.
func (o *AlertOrchestrator) OnDumpReady(result *silencedump.EncodeResult) {
	if result == nil {
		slog.Warn("audio dump callback delivered no result; dump ignored")
		return
	}
	key := incidentKey{trigger: result.Trigger, incidentID: result.IncidentID}

	o.mu.Lock()
	pending := o.pendingRecoveries[key]
	delete(o.pendingRecoveries, key)
	ctx := o.notifyCtx
	o.mu.Unlock()

	if pending == nil {
		slog.Debug("audio dump ready but no pending recovery; dump ignored",
			"trigger", key.trigger, "incident_id", key.incidentID)
		return
	}

	pending.dumpData.Result = result
	now := time.Now()
	o.dispatcher.DispatchAudioDump(ctx, pending.activeChannels, pending.cfg, pending.dumpData)
	o.enqueueLog("audio_dump_ready", func() {
		o.logAudioDumpReady(now, &pending.dumpData)
	})
}

// HandleUploadAbandoned dispatches an upload-abandonment alert outside the audio lifecycle.
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
	o.imbalanceActiveChannels = nil
	clear(o.pendingRecoveries)
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

// BuildGraphConfig extracts Microsoft Graph email settings from a config snapshot.
func BuildGraphConfig(cfg *config.Snapshot) *GraphConfig {
	return &GraphConfig{
		TenantID:     cfg.GraphTenantID,
		ClientID:     cfg.GraphClientID,
		ClientSecret: cfg.GraphClientSecret,
		FromAddress:  cfg.GraphFromAddress,
		Recipients:   cfg.GraphRecipients,
	}
}

func (o *AlertOrchestrator) logSilenceStart(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID, levelL, levelR float64,
) {
	if err := o.eventLogger.LogSilenceStart(t, uint32(incidentID), levelL, levelR, cfg.SilenceThreshold); err != nil {
		slog.Warn("failed to log silence start", "error", err)
	}
}

func (o *AlertOrchestrator) logSilenceEnd(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID, durationMS int64, levelL, levelR float64,
) {
	if err := o.eventLogger.LogSilenceEnd(
		t, uint32(incidentID), durationMS, levelL, levelR, cfg.SilenceThreshold,
	); err != nil {
		slog.Warn("failed to log silence end", "error", err)
	}
}

func (o *AlertOrchestrator) logChannelImbalanceStart(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	if err := o.eventLogger.LogChannelImbalanceStart(
		t, uint32(incidentID), levelL, levelR, balanceDB, imbalanceDB, cfg.ChannelImbalanceThreshold,
	); err != nil {
		slog.Warn("failed to log channel imbalance start", "error", err)
	}
}

func (o *AlertOrchestrator) logChannelImbalanceEnd(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID,
	durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	if err := o.eventLogger.LogChannelImbalanceEnd(
		t, uint32(incidentID), durationMS, levelL, levelR, balanceDB, imbalanceDB, cfg.ChannelImbalanceThreshold,
	); err != nil {
		slog.Warn("failed to log channel imbalance end", "error", err)
	}
}

func (o *AlertOrchestrator) logAudioDumpReady(t time.Time, data *AudioDumpData) {
	details := &eventlog.AudioDumpDetails{
		Trigger:      string(data.Trigger),
		IncidentID:   uint32(data.IncidentID),
		LevelLeftDB:  data.LevelL,
		LevelRightDB: data.LevelR,
		BalanceDB:    data.BalanceDB,
		ImbalanceDB:  data.ImbalanceDB,
		ThresholdDB:  data.ThresholdDB,
		DurationMs:   data.DurationMs,
	}
	if data.Result.Error != nil {
		details.DumpError = data.Result.Error.Error()
	} else {
		details.DumpPath = data.Result.FilePath
		details.DumpFilename = data.Result.Filename
		details.DumpSizeBytes = data.Result.FileSize
	}
	if err := o.eventLogger.LogAudioDumpReady(t, details); err != nil {
		slog.Warn("failed to log audio dump ready", "error", err)
	}
}
