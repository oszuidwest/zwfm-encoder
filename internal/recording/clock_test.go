package recording

import (
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// fakeClockSync makes the recorder wait until the returned flag is set.
// Tests using it must not be parallel.
func fakeClockSync(t *testing.T) *atomic.Bool {
	t.Helper()
	var synced atomic.Bool
	previous := clockSynced
	clockSynced, clockSyncPoll = synced.Load, 10*time.Millisecond
	t.Cleanup(func() { clockSynced, clockSyncPoll = previous, time.Second })
	return &synced
}

func TestRecorderWaitsForClockSync(t *testing.T) {
	synced := fakeClockSync(t)
	cfg := localOnDemandRecorder(t, "clock")
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop() })

	time.Sleep(100 * time.Millisecond)
	if status := r.Status(); status.State != types.ProcessStarting || status.Error == "" {
		t.Fatalf("status before clock sync = %+v, want starting with a reason", status)
	}
	if entries, err := os.ReadDir(cfg.LocalPath); err != nil || len(entries) != 0 {
		t.Fatalf("files before clock sync = %v, %v; want none", entries, err)
	}
	synced.Store(true)
	waitForRecorderState(t, r, types.ProcessRunning)
	if status := r.Status(); status.Error != "" {
		t.Fatalf("error after clock sync = %q, want cleared", status.Error)
	}
}

func TestStopRecorderCancelsClockWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		synced := fakeClockSync(t)
		cfg := localOnDemandRecorder(t, "clock-cancel")
		cfg.Enabled = true
		m, err := NewManager("", t.TempDir(), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.AddRecorder(cfg); err != nil {
			t.Fatal(err)
		}
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = m.Stop() }()
		if err := m.StartRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // The start now sleeps in its clock poll.
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatalf("StopRecorder while waiting for the clock = %v", err)
		}
		synced.Store(true)
		time.Sleep(clockSyncPoll)
		synctest.Wait() // The cancelled start has polled again; synctest fails if it never returns.
		if state := m.recorders[cfg.ID].Status().State; state != types.ProcessStopped {
			t.Fatalf("state after a cancelled start = %q, want stopped", state)
		}
	})
}
