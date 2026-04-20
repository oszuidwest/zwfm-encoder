package notify

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// testChannel is a stub AlertChannel that records which Send methods were called.
// Calls are signalled on buffered channels so tests can synchronise with dispatch goroutines.
type testChannel struct {
	name              string
	configuredSilence bool
	subscribesStart   bool
	subscribesEnd     bool
	subscribesDump    bool

	isConfiguredCalls int // counts IsConfiguredForSilence calls, guarded by the test being single-threaded up to dispatch

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
func (c *testChannel) IsConfiguredForUpload(_ *config.Snapshot) bool  { return false }
func (c *testChannel) SubscribesSilenceStart(_ *config.Snapshot) bool { return c.subscribesStart }
func (c *testChannel) SubscribesSilenceEnd(_ *config.Snapshot) bool   { return c.subscribesEnd }
func (c *testChannel) SubscribesAudioDump(_ *config.Snapshot) bool    { return c.subscribesDump }
func (c *testChannel) SendUploadAbandoned(_ *config.Snapshot, _ UploadAbandonedData) error {
	return nil
}

func (c *testChannel) IsConfiguredForSilence(_ *config.Snapshot) bool {
	c.isConfiguredCalls++
	return c.configuredSilence
}

func (c *testChannel) SendSilenceStart(cfg *config.Snapshot, _, _ float64) error {
	c.silenceStartCalled <- cfg
	return nil
}

func (c *testChannel) SendSilenceEnd(cfg *config.Snapshot, _ int64, _, _ float64) error {
	c.silenceEndCalled <- cfg
	return nil
}

func (c *testChannel) SendAudioDump(cfg *config.Snapshot, _ int64, _, _ float64, _ *silencedump.EncodeResult) error {
	c.audioDumpCalled <- cfg
	return nil
}

// awaitCall waits for a signal on ch or fails the test after a short timeout.
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

// assertNoCall asserts that no call arrives on ch within a brief window.
func assertNoCall(t *testing.T, ch <-chan *config.Snapshot, label string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected call: %s", label)
	case <-time.After(50 * time.Millisecond):
	}
}

// newTestOrchestrator creates an AlertOrchestrator backed by ch and registers t.Cleanup
// to drain and stop the log worker when the test ends, preventing goroutine leaks.
func newTestOrchestrator(t *testing.T, ch AlertChannel) *AlertOrchestrator {
	t.Helper()
	cfg := config.New("") // in-memory defaults, no file I/O
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(func() { o.DrainLogs(); close(o.logQueue) })
	return o
}

// TestOnDumpReadyNilPending verifies that OnDumpReady is a no-op when there is no
// pending recovery (e.g. called after Reset, or before any silence event).
func TestOnDumpReadyNilPending(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	o := newTestOrchestrator(t, ch)

	o.OnDumpReady(nil)

	assertNoCall(t, ch.audioDumpCalled, "SendAudioDump")
}

// TestOnDumpReadyNilPendingAfterReset verifies that a Reset between silence recovery and
// the dump callback causes OnDumpReady to no-op rather than panic or dispatch stale state.
func TestOnDumpReadyNilPendingAfterReset(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	o := newTestOrchestrator(t, ch)

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")

	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")

	// Encoder stop clears pending state before dump callback fires.
	o.Reset()
	o.OnDumpReady(nil)

	assertNoCall(t, ch.audioDumpCalled, "SendAudioDump after Reset")
}

// TestActiveChannelsClearedAfterRecovery verifies that after a silence period ends,
// activeChannels is cleared so the next silence period rebuilds the channel set fresh.
func TestActiveChannelsClearedAfterRecovery(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)

	// Period 1
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart period 1")
	callsAfterPeriod1Start := ch.isConfiguredCalls

	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 3000})
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd period 1")

	// Period 2: IsConfiguredForSilence must be called again to rebuild activeChannels.
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart period 2")

	if ch.isConfiguredCalls <= callsAfterPeriod1Start {
		t.Fatal("expected IsConfiguredForSilence to be called again for period 2, but it was not")
	}
}

// TestActiveChannelsNotRebuiltWithinSilencePeriod verifies that duplicate JustEntered events
// within the same silence period do not re-evaluate the channel set.
func TestActiveChannelsNotRebuiltWithinSilencePeriod(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart first")
	callsAfterFirst := ch.isConfiguredCalls

	// Second JustEntered during same period (abnormal but must not re-evaluate channels).
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart second")

	if ch.isConfiguredCalls != callsAfterFirst {
		t.Fatalf("IsConfiguredForSilence called %d times after first entry, want %d (no rebuild within period)",
			ch.isConfiguredCalls, callsAfterFirst)
	}
}

// TestAudioDumpUsesSnapshotFromSilenceEnd verifies that the config snapshot passed to
// SendAudioDump is the one captured at silence-end time, not a fresh snapshot taken later.
// This guards against the regression where OnDumpReady called o.cfg.Snapshot() instead of
// reusing pending.cfg.
func TestAudioDumpUsesSnapshotFromSilenceEnd(t *testing.T) {
	t.Parallel()
	ch := newTestChannel(true)
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(func() { o.DrainLogs(); close(o.logQueue) })

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")

	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	silenceEndSnap := awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")

	// Change the threshold in config after silence-end. A fresh Snapshot() would reflect
	// the new value; pending.cfg must not.
	if err := cfg.ApplySettings(&config.SettingsUpdate{
		SilenceThreshold:         -20,
		SilenceDurationMs:        15000,
		SilenceRecoveryMs:        5000,
		SilenceDumpEnabled:       true,
		SilenceDumpRetentionDays: 7,
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

// TestLogWriteOrder verifies that silence_start is physically written before silence_end in
// the JSONL file. Both events are enqueued back-to-back so the log worker receives them
// together and must write them in FIFO order — the same condition that caused the original
// out-of-order entries when two goroutines raced on Logger.mu.
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
	t.Cleanup(func() { o.DrainLogs(); close(o.logQueue) })
	o.SetEventLogger(logger)

	// Fire both events without an intervening barrier. The log jobs are enqueued in call
	// order; the worker writes them in FIFO order regardless of when it runs.
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, TotalDurationMs: 5000})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")
	awaitCall(t, ch.silenceEndCalled, "SendSilenceEnd")

	// Wait for all queued log writes to complete before reading the file.
	o.DrainLogs()

	events, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// ReadLast returns newest first (reverse file order).
	// Correct physical order means silence_end is last in file → first returned.
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

// TestDrainLogs verifies that DrainLogs blocks until all pending log jobs have been
// executed, so callers can rely on entries being flushed to the file after it returns.
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
	t.Cleanup(func() { o.DrainLogs(); close(o.logQueue) })
	o.SetEventLogger(logger)

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true})
	awaitCall(t, ch.silenceStartCalled, "SendSilenceStart")

	// DrainLogs must block until all pending log writes complete.
	o.DrainLogs()

	// The entry must be visible in the file immediately after drain returns.
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

// TestEnqueueLogDropsWhenFull verifies that enqueueLog drops entries without panicking
// when the log queue is at capacity (logQueueDepth), and that entries already queued
// before the overflow are not lost.
func TestEnqueueLogDropsWhenFull(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown

	ch := newTestChannel(false)
	o := newTestOrchestrator(t, ch)
	o.SetEventLogger(logger)

	// Block the worker so the queue fills up.
	block := make(chan struct{})
	o.enqueueLog(func() { <-block })

	// Fill the remaining logQueueDepth-1 slots.
	var wrote atomic.Int32
	for range logQueueDepth - 1 {
		o.enqueueLog(func() { wrote.Add(1) })
	}

	// Queue is at capacity; this entry must be dropped, not panic.
	o.enqueueLog(func() { wrote.Add(1) })

	close(block)
	o.DrainLogs()

	if got := wrote.Load(); got != int32(logQueueDepth-1) {
		t.Errorf("expected %d writes (one dropped), got %d", logQueueDepth-1, got)
	}
}

// TestLogWriteOrderWithDump verifies that a complete silence cycle writes all three
// JSONL events — silence_start, silence_end, audio_dump_ready — in that physical order.
func TestLogWriteOrderWithDump(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close() //nolint:errcheck // test teardown

	ch := newTestChannel(true) // subscribes to audio dump
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	o := NewAlertOrchestrator(cfg, NewDispatcher(ch))
	t.Cleanup(func() { o.DrainLogs(); close(o.logQueue) })
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

	// ReadLast returns newest first; correct physical order is start → end → dump.
	if events[0].Type != eventlog.AudioDumpReady {
		t.Errorf("events[0]: got %s, want %s", events[0].Type, eventlog.AudioDumpReady)
	}
	if events[1].Type != eventlog.SilenceEnd {
		t.Errorf("events[1]: got %s, want %s", events[1].Type, eventlog.SilenceEnd)
	}
	if events[2].Type != eventlog.SilenceStart {
		t.Errorf("events[2]: got %s, want %s", events[2].Type, eventlog.SilenceStart)
	}
	// Timestamps must be non-decreasing (start ≤ end ≤ dump).
	if events[2].Timestamp.After(events[1].Timestamp) {
		t.Errorf("silence_start (%v) after silence_end (%v)", events[2].Timestamp, events[1].Timestamp)
	}
	if events[1].Timestamp.After(events[0].Timestamp) {
		t.Errorf("silence_end (%v) after audio_dump_ready (%v)", events[1].Timestamp, events[0].Timestamp)
	}
}
