package encoder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
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
