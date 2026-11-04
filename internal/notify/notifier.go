// Package notify handles event notifications across multiple channels.
package notify

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// logQueueDepth buffers ordered event-log writes from audio cycle handlers.
const logQueueDepth = 16

// inputQueueDepth buffers input alerts per channel. Outages are rare, so a full
// queue means the channel is stuck; the oldest alerts then make way.
const inputQueueDepth = 8

// pendingRecoveryTTL keeps recovery context through capture and encoding delays.
const pendingRecoveryTTL = time.Minute

// logJob is a queued event-log write.
// eventType keeps queue-full diagnostics specific without inspecting the closure.
type logJob struct {
	eventType string // event type label for diagnostics; empty for internal sentinels
	fn        func()
}

// AlertOrchestrator coordinates silence, channel-imbalance, audio-dump,
// audio-input, recording-storage and upload-overdue alert lifecycles.
type AlertOrchestrator struct {
	cfg         *config.Config
	eventLogger *eventlog.Logger
	dispatcher  *Dispatcher

	mu                      sync.Mutex
	activeChannels          []AlertChannel
	imbalanceActiveChannels []AlertChannel
	activeCfg               config.Snapshot // taken with activeChannels; delivers the incident's end and dump
	imbalanceActiveCfg      config.Snapshot // taken with imbalanceActiveChannels; delivers its end and dump
	silenceIncident         audio.IncidentID
	imbalanceIncident       audio.IncidentID
	pendingRecoveries       map[incidentKey]*pendingRecoveryData
	notifyCtx               context.Context
	notifyCancel            context.CancelFunc

	logMu       sync.RWMutex
	logQueue    chan logJob   // serialized log writes; single worker guarantees JSONL write order
	inputQueues []chan func() // per-channel input alert deliveries, index-aligned with dispatcher.Channels()
	// silenceQueues keep each channel's silence start, end and dump in order.
	silenceQueues []chan func()
	// imbalanceQueues keep each channel's imbalance start, end and dump in order.
	imbalanceQueues []chan func()
	// storageQueues hold each channel's newest storage state, so input alerts
	// cannot evict a storage transition that is only sent once.
	storageQueues []chan func()
	// uploadQueues hold each channel's newest upload overdue state.
	uploadQueues []chan func()
	closeOnce    sync.Once // ensures Close drains and stops the worker exactly once
	closed       bool
}

// pendingRecoveryData holds recovery event data while waiting for the audio dump.
type pendingRecoveryData struct {
	dumpData       AudioDumpData
	cfg            config.Snapshot // captured at incident start; reused for dump delivery
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
	// One FIFO worker per channel delivers input alerts in the order they were
	// raised, so a restoration never lands before its loss and an old
	// restoration never lands after a newer loss and clears a live outage.
	// Silence and imbalance alerts each get their own worker for the same reason,
	// so a blocked delivery for one does not stall the other.
	for range dispatcher.Channels() {
		input, storage, upload := make(chan func(), inputQueueDepth), make(chan func(), 1), make(chan func(), 1)
		silence, imbalance := make(chan func(), inputQueueDepth), make(chan func(), inputQueueDepth)
		o.inputQueues = append(o.inputQueues, input)
		o.silenceQueues = append(o.silenceQueues, silence)
		o.imbalanceQueues = append(o.imbalanceQueues, imbalance)
		o.storageQueues = append(o.storageQueues, storage)
		o.uploadQueues = append(o.uploadQueues, upload)
		for _, queue := range []chan func(){input, storage, upload, silence, imbalance} {
			go func() {
				for deliver := range queue {
					deliver()
				}
			}()
		}
	}
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
		o.activeCfg = cfg
	}
	o.silenceIncident = incidentID
	active := o.activeChannels
	o.mu.Unlock()

	now := time.Now()
	o.enqueueOrdered(o.silenceQueues, cfg, "silence_start", subscribedActive(active, AlertChannel.SubscribesSilenceStart),
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendSilenceStart(ctx, cfg, levelL, levelR)
		})
	o.enqueueLog("silence_start", func() { o.logSilenceStart(now, &cfg, incidentID, levelL, levelR) })
}

func (o *AlertOrchestrator) handleSilenceEnd(
	incidentID audio.IncidentID, durationMS int64, levelL, levelR float64,
) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	active, startCfg := o.activeChannels, o.activeCfg
	o.activeChannels = nil
	o.silenceIncident = 0
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
		cfg:            startCfg,
		activeChannels: active,
		recoveredAt:    now,
	})
	o.mu.Unlock()

	startCfg.SilenceThreshold = cfg.SilenceThreshold // The end reports the threshold it recovered against.
	o.enqueueOrdered(o.silenceQueues, startCfg, "silence_end", subscribedActive(active, AlertChannel.SubscribesSilenceEnd),
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendSilenceEnd(ctx, cfg, durationMS, levelL, levelR)
		})
	o.enqueueLog("silence_end", func() { o.logSilenceEnd(now, &cfg, incidentID, durationMS, levelL, levelR) })
}

// subscribedActive limits ordered incident delivery to the incident's channel set.
func subscribedActive(
	active []AlertChannel, subscribes func(AlertChannel, *config.Snapshot) bool,
) func(AlertChannel, *config.Snapshot) bool {
	return func(ch AlertChannel, cfg *config.Snapshot) bool {
		return slices.Contains(active, ch) && subscribes(ch, cfg)
	}
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
		o.imbalanceActiveCfg = cfg
	}
	o.imbalanceIncident = incidentID
	active := o.imbalanceActiveChannels
	o.mu.Unlock()

	data := ChannelImbalanceData{
		LevelL:      levelL,
		LevelR:      levelR,
		BalanceDB:   balanceDB,
		ImbalanceDB: imbalanceDB,
		ThresholdDB: cfg.ChannelImbalanceThreshold,
	}
	now := time.Now()
	o.enqueueOrdered(o.imbalanceQueues, cfg, "channel_imbalance_start",
		subscribedActive(active, AlertChannel.SubscribesChannelImbalanceStart),
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendChannelImbalanceStart(ctx, cfg, data)
		})
	o.enqueueLog("channel_imbalance_start", func() {
		o.logChannelImbalanceStart(now, &cfg, incidentID, levelL, levelR, balanceDB, imbalanceDB)
	})
}

func (o *AlertOrchestrator) handleChannelImbalanceEnd(
	incidentID audio.IncidentID, durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	cfg := o.cfg.Snapshot()

	o.mu.Lock()
	active, startCfg := o.imbalanceActiveChannels, o.imbalanceActiveCfg
	o.imbalanceActiveChannels = nil
	o.imbalanceIncident = 0
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
		cfg:            startCfg,
		activeChannels: active,
		recoveredAt:    now,
	})
	o.mu.Unlock()

	data := ChannelImbalanceData{
		LevelL:      levelL,
		LevelR:      levelR,
		BalanceDB:   balanceDB,
		ImbalanceDB: imbalanceDB,
		ThresholdDB: cfg.ChannelImbalanceThreshold,
		DurationMs:  durationMS,
	}
	o.enqueueOrdered(o.imbalanceQueues, startCfg, "channel_imbalance_end",
		subscribedActive(active, AlertChannel.SubscribesChannelImbalanceEnd),
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendChannelImbalanceEnd(ctx, cfg, data)
		})
	o.enqueueLog("channel_imbalance_end", func() {
		o.logChannelImbalanceEnd(now, &cfg, incidentID, durationMS, levelL, levelR, balanceDB, imbalanceDB)
	})
}

// addPendingRecoveryLocked retains recovery context for dump callbacks and prunes stale entries.
// The caller must hold o.mu.
func (o *AlertOrchestrator) addPendingRecoveryLocked(pending *pendingRecoveryData) {
	cutoff := pending.recoveredAt.Add(-pendingRecoveryTTL)
	maps.DeleteFunc(o.pendingRecoveries, func(_ incidentKey, existing *pendingRecoveryData) bool {
		return existing.recoveredAt.Before(cutoff)
	})
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
	now := time.Now()

	o.mu.Lock()
	pending := o.pendingRecoveries[key]
	delete(o.pendingRecoveries, key)
	o.mu.Unlock()

	if pending == nil || now.Sub(pending.recoveredAt) > pendingRecoveryTTL {
		slog.Debug("audio dump ready but no pending recovery; dump ignored",
			"trigger", key.trigger, "incident_id", key.incidentID)
		return
	}

	pending.dumpData.Result = result
	queues := o.silenceQueues
	if key.trigger == silencedump.TriggerChannelImbalance {
		queues = o.imbalanceQueues
	}
	o.enqueueOrdered(queues, pending.cfg, "audio_dump_ready",
		subscribedActive(pending.activeChannels, AlertChannel.SubscribesAudioDump),
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendAudioDump(ctx, cfg, &pending.dumpData)
		})
	o.enqueueLog("audio_dump_ready", func() {
		o.logAudioDumpReady(now, &pending.dumpData)
	})
}

// HandleUploadsOverdue sends the upload overdue state through the ordered
// channel workers, so a delayed alert cannot arrive after its recovery.
func (o *AlertOrchestrator) HandleUploadsOverdue(state *UploadsOverdueData) {
	data := *state
	data.Recorders = slices.Clone(state.Recorders)
	o.enqueueOrdered(o.uploadQueues, o.cfg.Snapshot(), "upload_overdue", AlertChannel.IsConfiguredForUpload,
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendUploadsOverdue(ctx, cfg, &data)
		})
}

// HandleRecordingStorage sends storage changes through the ordered channel workers,
// so a delayed low-space notification cannot arrive after its recovery.
func (o *AlertOrchestrator) HandleRecordingStorage(paths []types.RecordingStorageStatus) {
	paths = slices.Clone(paths)
	o.enqueueOrdered(o.storageQueues, o.cfg.Snapshot(), "recording_storage", AlertChannel.IsConfiguredForUpload,
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return ch.SendRecordingStorage(ctx, cfg, paths)
		})
}

// HandleInputLost alerts every channel configured for audio alerts and logs an
// audio input loss. Calls must be ordered with [AlertOrchestrator.HandleInputRestored].
func (o *AlertOrchestrator) HandleInputLost(data InputData) {
	o.handleInputEvent("input_lost", data, AlertChannel.SendInputLost, (*eventlog.Logger).LogInputLost)
}

// HandleInputRestored alerts every channel configured for audio alerts and
// logs an audio input restoration.
func (o *AlertOrchestrator) HandleInputRestored(data InputData) {
	o.handleInputEvent("input_restored", data, AlertChannel.SendInputRestored, (*eventlog.Logger).LogInputRestored)
}

// handleInputEvent queues the alert on each channel's ordered worker. Like
// recording-storage and upload alerts, input alerts ignore per-event subscriptions.
func (o *AlertOrchestrator) handleInputEvent(
	eventType string, data InputData,
	send func(AlertChannel, context.Context, *config.Snapshot, InputData) error,
	write func(*eventlog.Logger, time.Time, *eventlog.InputDetails) error,
) {
	now := time.Now()
	o.enqueueOrdered(o.inputQueues, o.cfg.Snapshot(), eventType, AlertChannel.IsConfiguredForSilence,
		func(ch AlertChannel, ctx context.Context, cfg *config.Snapshot) error {
			return send(ch, ctx, cfg, data)
		})

	o.enqueueLog(eventType, func() {
		if err := write(o.eventLogger, now, &eventlog.InputDetails{
			Input:      data.Input,
			Error:      data.Error,
			DurationMs: data.DurationMs,
		}); err != nil {
			slog.Warn("failed to log audio input event", "event_type", eventType, "error", err)
		}
	})
}

// enqueueOrdered queues send on each configured channel's FIFO worker.
// Each channel gets its own Snapshot copy.
//
//nolint:gocritic // hugeParam: intentional; callers pass the snapshot their event was raised with
func (o *AlertOrchestrator) enqueueOrdered(
	queues []chan func(), cfg config.Snapshot, eventType string, configured func(AlertChannel, *config.Snapshot) bool,
	send func(AlertChannel, context.Context, *config.Snapshot) error,
) {
	o.mu.Lock()
	ctx := o.notifyCtx
	o.mu.Unlock()

	o.logMu.RLock()
	defer o.logMu.RUnlock()
	if o.closed {
		return
	}
	for i, ch := range o.dispatcher.Channels() {
		if !configured(ch, &cfg) {
			continue
		}
		chCfg := cfg
		enqueueInputAlert(queues[i], func() {
			logNotifyResult(func() error { return send(ch, ctx, &chCfg) }, ch.Name(), eventType)
		}, ch.Name(), eventType)
	}
}

// enqueueInputAlert queues deliver, discarding the oldest queued alert when
// the channel is backed up: the newest state matters most to a monitor that
// shows the last value, so the latest loss or restoration is always kept.
func enqueueInputAlert(queue chan func(), deliver func(), channel, eventType string) {
	for {
		select {
		case queue <- deliver:
			return
		default:
		}
		select {
		case <-queue:
			slog.Warn("input alert queue full, oldest alert dropped", "channel", channel, "event_type", eventType)
		default:
		}
	}
}

// Reset clears alert state for the current silence period, including any pending dump dispatch.
// In-flight notifications are intentionally NOT cancelled here: Reset runs on encoder
// start and stop, and a short silence followed by a Stop would otherwise abort
// a still-pending silence_start delivery. The notify context is cancelled only on Close.
// An incident still open ends as stopped in the event log; receivers get no alert for it.
func (o *AlertOrchestrator) Reset() {
	now := time.Now()
	o.mu.Lock()
	silence, imbalance := o.silenceIncident, o.imbalanceIncident
	o.silenceIncident, o.imbalanceIncident = 0, 0
	o.activeChannels = nil
	o.imbalanceActiveChannels = nil
	o.activeCfg = config.Snapshot{}
	o.imbalanceActiveCfg = config.Snapshot{}
	clear(o.pendingRecoveries)
	o.mu.Unlock()

	o.logIncidentStopped(now, eventlog.SilenceEnd, silence)
	o.logIncidentStopped(now, eventlog.ChannelImbalanceEnd, imbalance)
}

// logIncidentStopped queues the end of an incident left open by a stop; ID 0 means none is open.
func (o *AlertOrchestrator) logIncidentStopped(t time.Time, end eventlog.EventType, incidentID audio.IncidentID) {
	if incidentID == 0 {
		return
	}
	o.enqueueLog(string(end), func() {
		if err := o.eventLogger.LogIncidentStopped(t, end, uint32(incidentID)); err != nil {
			slog.Warn("failed to log stopped incident", "event_type", end, "error", err)
		}
	})
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
		for _, queue := range slices.Concat(o.inputQueues, o.storageQueues, o.uploadQueues, o.silenceQueues, o.imbalanceQueues) {
			close(queue)
		}
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
	if err := o.eventLogger.LogSilenceStart(t, &eventlog.SilenceDetails{
		IncidentID:   uint32(incidentID),
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		ThresholdDB:  cfg.SilenceThreshold,
	}); err != nil {
		slog.Warn("failed to log silence start", "error", err)
	}
}

func (o *AlertOrchestrator) logSilenceEnd(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID, durationMS int64, levelL, levelR float64,
) {
	if err := o.eventLogger.LogSilenceEnd(t, &eventlog.SilenceDetails{
		IncidentID:   uint32(incidentID),
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		ThresholdDB:  cfg.SilenceThreshold,
		DurationMs:   durationMS,
	}); err != nil {
		slog.Warn("failed to log silence end", "error", err)
	}
}

func (o *AlertOrchestrator) logChannelImbalanceStart(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	if err := o.eventLogger.LogChannelImbalanceStart(t, &eventlog.ImbalanceDetails{
		IncidentID:   uint32(incidentID),
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		BalanceDB:    balanceDB,
		ImbalanceDB:  imbalanceDB,
		ThresholdDB:  cfg.ChannelImbalanceThreshold,
	}); err != nil {
		slog.Warn("failed to log channel imbalance start", "error", err)
	}
}

func (o *AlertOrchestrator) logChannelImbalanceEnd(
	t time.Time, cfg *config.Snapshot, incidentID audio.IncidentID,
	durationMS int64, levelL, levelR, balanceDB, imbalanceDB float64,
) {
	if err := o.eventLogger.LogChannelImbalanceEnd(t, &eventlog.ImbalanceDetails{
		IncidentID:   uint32(incidentID),
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		BalanceDB:    balanceDB,
		ImbalanceDB:  imbalanceDB,
		ThresholdDB:  cfg.ChannelImbalanceThreshold,
		DurationMs:   durationMS,
	}); err != nil {
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
