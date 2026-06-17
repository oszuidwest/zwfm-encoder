package notify

import (
	"context"
	"errors"
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type noopAlertChannel struct{}

func (noopAlertChannel) Name() string                                   { return "noop" }
func (noopAlertChannel) IsConfiguredForSilence(_ *config.Snapshot) bool { return false }
func (noopAlertChannel) IsConfiguredForUpload(_ *config.Snapshot) bool  { return false }
func (noopAlertChannel) SubscribesSilenceStart(_ *config.Snapshot) bool { return false }
func (noopAlertChannel) SubscribesSilenceEnd(_ *config.Snapshot) bool   { return false }
func (noopAlertChannel) SubscribesAudioDump(_ *config.Snapshot) bool    { return false }
func (noopAlertChannel) SendSilenceStart(_ context.Context, _ *config.Snapshot, _, _ float64) error {
	return nil
}
func (noopAlertChannel) SendSilenceEnd(_ context.Context, _ *config.Snapshot, _ int64, _, _ float64) error {
	return nil
}
func (noopAlertChannel) SendAudioDump(
	_ context.Context, _ *config.Snapshot, _ int64, _, _ float64, _ *silencedump.EncodeResult,
) error {
	return nil
}
func (noopAlertChannel) SendUploadAbandoned(_ context.Context, _ *config.Snapshot, _ UploadAbandonedData) error {
	return nil
}

type silenceStartChannel struct{ noopAlertChannel }

func (silenceStartChannel) IsConfiguredForSilence(_ *config.Snapshot) bool { return true }
func (silenceStartChannel) SubscribesSilenceStart(_ *config.Snapshot) bool { return true }

type snapshotMutatingChannel struct {
	silenceStartChannel
	mutated chan struct{}
	release chan struct{}
}

func (c *snapshotMutatingChannel) SendSilenceStart(_ context.Context, cfg *config.Snapshot, _, _ float64) error {
	cfg.SilenceThreshold = -20
	close(c.mutated)
	<-c.release
	return nil
}

type snapshotObservingChannel struct {
	silenceStartChannel
	mutated   chan struct{}
	observed  chan float64
	releaseMu chan struct{}
}

func (c *snapshotObservingChannel) SendSilenceStart(_ context.Context, cfg *config.Snapshot, _, _ float64) error {
	<-c.mutated
	c.observed <- cfg.SilenceThreshold
	close(c.releaseMu)
	return nil
}

type contextCapturingChannel struct {
	silenceStartChannel
	ctx         chan context.Context
	canceled    chan struct{}
	done        chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newContextCapturingChannel() *contextCapturingChannel {
	return &contextCapturingChannel{
		ctx:      make(chan context.Context, 1),
		canceled: make(chan struct{}),
		done:     make(chan struct{}),
		release:  make(chan struct{}),
	}
}
func (c *contextCapturingChannel) SendSilenceStart(ctx context.Context, _ *config.Snapshot, _, _ float64) error {
	defer close(c.done)
	c.ctx <- ctx
	select {
	case <-ctx.Done():
		close(c.canceled)
	case <-c.release:
	}
	return nil
}
func (c *contextCapturingChannel) releaseSend() {
	c.releaseOnce.Do(func() { close(c.release) })
}

type captureHandler struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler requires slog.Record by value.
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level != slog.LevelWarn || r.Message != "log queue full, log entry dropped" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attrs = h.attrs[:0]
	r.Attrs(func(attr slog.Attr) bool {
		h.attrs = append(h.attrs, attr)
		return true
	})
	return nil
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{attrs: append([]slog.Attr(nil), attrs...)}
}
func (h *captureHandler) WithGroup(string) slog.Handler { return h }
func (h *captureHandler) attrValue(key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, attr := range h.attrs {
		if attr.Key == key {
			return attr.Value.String(), true
		}
	}
	return "", false
}

type testChannel struct {
	noopAlertChannel
	name               string
	configuredSilence  bool
	subscribesStart    bool
	subscribesEnd      bool
	subscribesDump     bool
	isConfiguredCalls  int // counts IsConfiguredForSilence calls, guarded by the test being single-threaded up to dispatch
	silenceStartCalled chan *config.Snapshot
	silenceEndCalled   chan *config.Snapshot
	audioDumpCalled    chan *config.Snapshot
}

func newTestChannel(subscribesDump bool) *testChannel {
	return &testChannel{
		name:               "test",
		configuredSilence:  true,
		subscribesStart:    true,
		subscribesEnd:      true,
		subscribesDump:     subscribesDump,
		silenceStartCalled: make(chan *config.Snapshot, 1),
		silenceEndCalled:   make(chan *config.Snapshot, 1),
		audioDumpCalled:    make(chan *config.Snapshot, 1),
	}
}
func (c *testChannel) Name() string                                   { return c.name }
func (c *testChannel) SubscribesSilenceStart(_ *config.Snapshot) bool { return c.subscribesStart }
func (c *testChannel) SubscribesSilenceEnd(_ *config.Snapshot) bool   { return c.subscribesEnd }
func (c *testChannel) SubscribesAudioDump(_ *config.Snapshot) bool    { return c.subscribesDump }
func (c *testChannel) IsConfiguredForSilence(_ *config.Snapshot) bool {
	c.isConfiguredCalls++
	return c.configuredSilence
}
func (c *testChannel) SendSilenceStart(_ context.Context, cfg *config.Snapshot, _, _ float64) error {
	c.silenceStartCalled <- cfg
	return nil
}
func (c *testChannel) SendSilenceEnd(_ context.Context, cfg *config.Snapshot, _ int64, _, _ float64) error {
	c.silenceEndCalled <- cfg
	return nil
}
func (c *testChannel) SendAudioDump(
	_ context.Context, cfg *config.Snapshot, _ int64, _, _ float64, _ *silencedump.EncodeResult,
) error {
	c.audioDumpCalled <- cfg
	return nil
}

func awaitCall(t *testing.T, ch <-chan *config.Snapshot, label string) *config.Snapshot {
	t.Helper()
	select {
	case snap := <-ch:
		return snap
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}

func assertNoCall(t *testing.T, ch <-chan *config.Snapshot, label string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected call: %s", label)
	case <-time.After(50 * time.Millisecond):
	}
}
func awaitContext(t *testing.T, ch <-chan context.Context, label string) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
func awaitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func newTestOrchestrator(t *testing.T, ch AlertChannel) *AlertOrchestrator {
	t.Helper()
	cfg := config.New("") // in-memory defaults, no file I/O
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	return o
}

func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)
	o.Close()
	o.Close() // must not panic (t.Cleanup will call it a third time)
}
func TestDrainLogsAfterCloseIsNoOp(t *testing.T) {
	t.Parallel()
	o := newTestOrchestrator(t, newTestChannel(false))
	o.Close()
	done := make(chan struct{})
	go func() {
		o.DrainLogs()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DrainLogs blocked after Close")
	}
}
func TestEnqueueLogAfterCloseIsNoOp(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown
	o := newTestOrchestrator(t, newTestChannel(false))
	o.SetEventLogger(logger)
	o.Close()
	var ran atomic.Bool
	o.enqueueLog("post_close", func() { ran.Store(true) })
	if ran.Load() {
		t.Fatal("enqueueLog executed after Close")
	}
}
func TestDrainLogsConcurrentWithCloseDoesNotPanic(t *testing.T) {
	t.Parallel()
	o := newTestOrchestrator(t, newTestChannel(false))
	block := make(chan struct{})
	started := make(chan struct{})
	o.logQueue <- logJob{fn: func() { close(started); <-block }}
	<-started
	for range logQueueDepth {
		o.logQueue <- logJob{fn: func() {}}
	}
	drained := make(chan struct{})
	go func() {
		o.DrainLogs()
		close(drained)
	}()
	closed := make(chan struct{})
	go func() {
		o.Close()
		close(closed)
	}()
	close(block)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("DrainLogs blocked when racing with Close")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked when racing with DrainLogs")
	}
}
func TestResetDoesNotCancelInFlightNotification(t *testing.T) {
	t.Parallel()
	ch := newContextCapturingChannel()
	o := newTestOrchestrator(t, ch)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	ctx := awaitContext(t, ch.ctx, "in-flight notification context")
	o.Reset()
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err() after Reset() = %v, want nil", err)
	}
	ch.releaseSend()
	awaitSignal(t, ch.done, "blocked notification send to finish")
}
func TestCloseCancelsInFlightNotification(t *testing.T) {
	t.Parallel()
	ch := newContextCapturingChannel()
	o := newTestOrchestrator(t, ch)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	ctx := awaitContext(t, ch.ctx, "in-flight notification context")
	o.Close()
	awaitSignal(t, ch.canceled, "in-flight notification context cancellation")
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx.Err() after Close() = %v, want %v", err, context.Canceled)
	}
	awaitSignal(t, ch.done, "blocked notification send to finish")
}

func TestOnDumpReadyNilPending(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	o := newTestOrchestrator(t, ch)
	o.OnDumpReady(nil)
	assertNoCall(t, ch.audioDumpCalled, "SendAudioDump")
}

func TestOnDumpReadyNilPendingAfterReset(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	o := newTestOrchestrator(t, ch)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")
	o.Reset()
	o.OnDumpReady(nil)
	assertNoCall(t, ch.audioDumpCalled, "SendAudioDump after Reset")
}

func TestActiveChannelsClearedAfterRecovery(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart period 1")
	callsAfterPeriod1Start := ch.isConfiguredCalls
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 3000})
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd period 1")
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart period 2")
	if ch.isConfiguredCalls <= callsAfterPeriod1Start {
		t.Fatal("expected IsConfiguredForSilence to be called again for period 2, but it was not")
	}
}

func TestActiveChannelsNotRebuiltWithinSilencePeriod(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart first")
	callsAfterFirst := ch.isConfiguredCalls
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart second")
	if ch.isConfiguredCalls != callsAfterFirst {
		t.Fatalf("IsConfiguredForSilence called %d times after first entry, want %d (no rebuild within period)",
			ch.isConfiguredCalls, callsAfterFirst)
	}
}

func TestAudioDumpUsesSnapshotFromSilenceEnd(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	silenceEndSnap := awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")
	if err := cfg.ApplySettings(&config.SettingsUpdate{
		SilenceThreshold:            -20,
		SilenceDurationMs:           15000,
		SilenceRecoveryMs:           5000,
		PeakHoldMs:                  config.DefaultPeakHoldMs,
		ChannelImbalanceThreshold:   config.DefaultChannelImbalanceThreshold,
		ChannelImbalanceDurationMs:  config.DefaultChannelImbalanceDurationMs,
		ChannelImbalanceRecoveryMs:  config.DefaultChannelImbalanceRecoveryMs,
		SilenceDumpEnabled:          true,
		SilenceDumpRetentionDays:    7,
		RecordingMaxDurationMinutes: config.DefaultRecordingMaxDurationMinutes,
	}); err != nil {
		t.Fatalf("ApplySettings failed: %v", err)
	}
	o.OnDumpReady(nil)
	audioDumpSnap := awaitCall(t, ch.audioDumpCalled, "SendAudioDump")
	if audioDumpSnap.SilenceThreshold != silenceEndSnap.SilenceThreshold {
		t.Fatalf("SendAudioDump received threshold %.1f dB, want %.1f dB (from silence-end snapshot)",
			audioDumpSnap.SilenceThreshold, silenceEndSnap.SilenceThreshold)
	}
}

func TestHandleChannelImbalanceEventLogsWithoutMutatingSilenceState(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown
	ch := newTestChannel(false)
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	o.SetEventLogger(logger)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	o.HandleChannelImbalanceEvent(&audio.ImbalanceEvent{
		JustEntered: true, ImbalanceDB: 40, BalanceDB: 40, CurrentLevelL: -6, CurrentLevelR: -46,
	})
	o.HandleChannelImbalanceEvent(&audio.ImbalanceEvent{
		JustRecovered: true, TotalDurationMs: 16000, CurrentLevelL: -6, CurrentLevelR: -8,
	})
	assertNoCall(t, ch.silenceStartCalled, "SendSilenceStart from imbalance event")
	assertNoCall(t, ch.silenceEndCalled, "SendSilenceEnd from imbalance event")
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 3000})
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")
	o.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var sawStart, sawEnd bool
	for _, e := range events {
		switch e.Type {
		case eventlog.ChannelImbalanceStart:
			sawStart = true
		case eventlog.ChannelImbalanceEnd:
			sawEnd = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("channel imbalance events not logged: start=%v end=%v (events=%+v)", sawStart, sawEnd, events)
	}
}

func TestZabbixChannelSendAudioDumpReturnsError(t *testing.T) {
	t.Parallel()
	ch := &ZabbixChannel{}
	err := ch.SendAudioDump(context.Background(), nil, 0, 0, 0, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "does not support audio dump delivery") {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestDispatchSilenceStartGivesEachGoroutineItsOwnSnapshotCopy(t *testing.T) {
	t.Parallel()
	mutated := make(chan struct{})
	releaseMutator := make(chan struct{})
	observed := make(chan float64, 1)
	releaseObserver := make(chan struct{})
	mutator := &snapshotMutatingChannel{
		mutated: mutated,
		release: releaseMutator,
	}
	observer := &snapshotObservingChannel{
		mutated:   mutated,
		observed:  observed,
		releaseMu: releaseObserver,
	}
	dispatcher := NewDispatcher(mutator, observer)
	cfg := config.Snapshot{SilenceThreshold: -40}
	dispatcher.DispatchSilenceStart(context.Background(), []AlertChannel{mutator, observer}, cfg, 0, 0)
	select {
	case got := <-observed:
		if got != -40 {
			t.Fatalf("observed SilenceThreshold = %.1f, want -40.0", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for observer channel")
	}
	<-releaseObserver
	close(releaseMutator)
}

func TestLogWriteOrder(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown
	ch := newTestChannel(false)
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	o.SetEventLogger(logger)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")
	o.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != eventlog.SilenceEnd {
		t.Errorf("events[0]: got %s, want %s", events[0].Type, eventlog.SilenceEnd)
	}
	if events[1].Type != eventlog.SilenceStart {
		t.Errorf("events[1]: got %s, want %s", events[1].Type, eventlog.SilenceStart)
	}
	if events[1].Timestamp.After(events[0].Timestamp) {
		t.Errorf("silence_start timestamp (%v) is after silence_end timestamp (%v)",
			events[1].Timestamp, events[0].Timestamp)
	}
}

func TestDrainLogs(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown
	ch := newTestChannel(false)
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	o.SetEventLogger(logger)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	o.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after drain, got %d", len(events))
	}
	if events[0].Type != eventlog.SilenceStart {
		t.Errorf("got %s, want %s", events[0].Type, eventlog.SilenceStart)
	}
}

func TestEnqueueLogDropsWhenFull(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown
	handler := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)
	o.SetEventLogger(logger)
	block := make(chan struct{})
	started := make(chan struct{})
	o.enqueueLog("test_block", func() { close(started); <-block })
	<-started // worker is now inside the blocking job; queue is empty
	var wrote atomic.Int32
	for range logQueueDepth {
		o.enqueueLog("test_write", func() { wrote.Add(1) })
	}
	o.enqueueLog("test_overflow", func() { wrote.Add(1) })
	close(block)
	o.DrainLogs()
	if got := wrote.Load(); got != int32(logQueueDepth) {
		t.Errorf("expected %d writes (one dropped), got %d", logQueueDepth, got)
	}
	if got, ok := handler.attrValue("event_type"); !ok || got != "test_overflow" {
		t.Fatalf("warning event_type = %q, %t; want %q, true", got, ok, "test_overflow")
	}
}

func TestLogWriteOrderWithDump(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close()       //nolint:errcheck // test teardown
	ch := newTestChannel(true) // subscribes to audio dump
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(o.Close)
	o.SetEventLogger(logger)
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	o.OnDumpReady(nil)
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")
	awaitCall(t, ch.audioDumpCalled, "SendAudioDump")
	o.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Type != eventlog.AudioDumpReady {
		t.Errorf("events[0]: got %s, want %s", events[0].Type, eventlog.AudioDumpReady)
	}
	if events[1].Type != eventlog.SilenceEnd {
		t.Errorf("events[1]: got %s, want %s", events[1].Type, eventlog.SilenceEnd)
	}
	if events[2].Type != eventlog.SilenceStart {
		t.Errorf("events[2]: got %s, want %s", events[2].Type, eventlog.SilenceStart)
	}
	if events[2].Timestamp.After(events[1].Timestamp) {
		t.Errorf("silence_start (%v) after silence_end (%v)", events[2].Timestamp, events[1].Timestamp)
	}
	if events[1].Timestamp.After(events[0].Timestamp) {
		t.Errorf("silence_end (%v) after audio_dump_ready (%v)", events[1].Timestamp, events[0].Timestamp)
	}
}
