package encoder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestInputOutageRestoredFromPreparedLog(t *testing.T) {
	for _, mode := range []string{"open", "closed", "none", "rotated"} {
		t.Run(mode, func(t *testing.T) {
			path, lostAt := prepareInputLog(t, mode)
			cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
			if err := cfg.Load(); err != nil {
				t.Fatal(err)
			}
			cfg.Audio.Input = "test-device"
			e, err := newEncoder(cfg, "ffmpeg", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Stop(); _ = e.Close() })
			e.buildCaptureCommand = helperCaptureCommand("pcm")
			e.inputStableDelay = 100 * time.Millisecond
			wantRecovery := mode == "open" || mode == "rotated"
			if wantRecovery && !e.inputLostAt.Equal(lostAt) {
				t.Fatalf("restored lost timestamp = %v, want %v", e.inputLostAt, lostAt)
			}
			if !wantRecovery && !e.inputLostAt.IsZero() {
				t.Fatalf("restored already-closed outage: %v", e.inputLostAt)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, 5*time.Second, "input confirmed after restart", func() bool {
				e.mu.RLock()
				id := e.sourceRunID
				e.mu.RUnlock()
				since, last := e.audioTimes(id)
				return !last.IsZero() && last.Sub(since) > time.Second
			})
			if err := e.Stop(); err != nil {
				t.Fatal(err)
			}
			e.alertOrchestrator.DrainLogs()
			events, _, err := eventlog.ReadLast(path, 3, 0, eventlog.FilterInput)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string]int{"open": 2, "closed": 2, "none": 0, "rotated": 2}[mode]; len(events) != want {
				t.Fatalf("input events = %+v, want %d events", events, want)
			}
			if wantRecovery {
				if events[0].Type != eventlog.InputRestored || events[1].Type != eventlog.InputLost {
					t.Fatalf("input events = %+v, want one restored/lost pair", events)
				}
				details, ok := events[0].Details.(map[string]any)
				if !ok {
					t.Fatalf("input_restored details = %T, want map", events[0].Details)
				}
				if duration, _ := details["duration_ms"].(float64); duration < 120000 {
					t.Fatalf("duration_ms = %v, want outage duration including restart", details["duration_ms"])
				}
			}
		})
	}
}

func prepareInputLog(t *testing.T, mode string) (string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	lostAt := time.Now().Add(-2 * time.Minute).UTC()
	if mode != "none" {
		if err := logger.LogInputLost(lostAt, &eventlog.InputDetails{Input: "old-input"}); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "closed" {
		if err := logger.LogInputRestored(lostAt.Add(time.Minute), &eventlog.InputDetails{Input: "old-input"}); err != nil {
			t.Fatal(err)
		}
	}
	// More unrelated audio events than one ReadLast page must not hide it.
	for range eventlog.MaxReadLimit + 1 {
		if err := logger.Log(&eventlog.Event{Type: eventlog.SilenceStart}); err != nil {
			t.Fatal(err)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if mode == "rotated" {
		if err := os.Rename(path, util.RotatedPath(path)); err != nil {
			t.Fatal(err)
		}
	}
	return path, lostAt
}

func TestUnreadableInputHistoryDoesNotBlockStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	// A directory where the rotated log belongs makes reading it fail.
	if err := os.Mkdir(util.RotatedPath(path), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	e, err := newEncoder(cfg, "ffmpeg", path)
	if err != nil {
		t.Fatalf("newEncoder() with an unreadable event log = %v, want a running encoder", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if !e.inputLostAt.IsZero() {
		t.Fatalf("inputLostAt = %v, want no restored outage", e.inputLostAt)
	}
}

func TestTruncatedLogDoesNotRepeatInputRecoveryAfterReopen(t *testing.T) {
	path, _ := prepareInputLog(t, "open")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // Test-controlled temporary log path.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"input_`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		e, err := newEncoder(cfg, "ffmpeg", path)
		if err != nil {
			t.Fatal(err)
		}
		if open := !e.inputLostAt.IsZero(); open != (attempt == 0) {
			_ = e.Close()
			t.Fatalf("reopen %d: open outage = %v", attempt, open)
		}
		e.state, e.sourceRunID = types.StateRunning, 1
		e.confirmInput(1, time.Now())
		e.alertOrchestrator.DrainLogs()
		e.state = types.StateStopped
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	events, _, err := eventlog.ReadLast(path, 10, 0, eventlog.FilterInput)
	if err != nil || len(events) != 2 || events[0].Type != eventlog.InputRestored || events[1].Type != eventlog.InputLost {
		t.Fatalf("events after two reopens = %+v, %v; want one recovery and one loss", events, err)
	}
}

func TestRestoredOutageFromAheadClockHasNoNegativeDuration(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	logPath := attachEventLog(t, e)
	e.state, e.sourceRunID = types.StateRunning, 1
	// The logged loss is ahead of a clock that has not synchronized yet.
	e.inputLostAt = time.Now().Add(time.Hour)
	e.confirmInput(1, time.Now())
	e.alertOrchestrator.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, 1, 0, eventlog.FilterInput)
	if err != nil || len(events) != 1 {
		t.Fatalf("input events = %+v, %v; want one input_restored", events, err)
	}
	if duration := events[0].Details.(map[string]any)["duration_ms"]; duration != nil && duration.(float64) < 0 {
		t.Fatalf("duration_ms = %v, want >= 0", duration)
	}
}
