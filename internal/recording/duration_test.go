package recording

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestModeChangesRescheduleRecordingBounds(t *testing.T) {
	neverRotate(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := localOnDemandRecorder(t, "mode-bounds")
	cfg.RecordingMode = types.RecordingHourly
	r := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg, FFmpegPath: executable, SpoolDir: t.TempDir(), MaxDurationMinutes: 60,
	})
	t.Cleanup(func() { _ = r.Stop() })
	startCaptureTestRecorder(t, r)
	var callbacks []func()
	var delays []time.Duration
	never := afterFunc
	afterFunc = func(d time.Duration, callback func()) *time.Timer {
		callbacks = append(callbacks, callback)
		delays = append(delays, d)
		return never(d, callback)
	}
	cfg.RecordingMode = types.RecordingOnDemand
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(callbacks) != 1 || delays[0] != time.Hour {
		t.Fatalf("on-demand timers = %v, want one 60-minute limit", delays)
	}
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(callbacks) != 1 {
		t.Fatal("an edit without a mode change extended the duration limit")
	}
	cfg.RecordingMode = types.RecordingHourly
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(callbacks) != 2 {
		t.Fatal("returning to hourly mode did not schedule a rotation")
	}
	callbacks[0]() // A duration callback already queued before the mode change.
	if r.Status().State != types.ProcessRunning {
		t.Fatal("the old duration limit stopped hourly recording")
	}
	before := r.currentFile
	callbacks[1]()
	if r.currentFile == before || len(callbacks) != 3 {
		t.Fatal("the hourly callback did not rotate and schedule the next file")
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	cfg.RecordingMode = types.RecordingOnDemand
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(callbacks) != 3 {
		t.Fatal("editing a stopped recorder scheduled a timer")
	}
}

func TestDurationLimitDiscardsQueuedCallbackAfterWriteError(t *testing.T) {
	for _, limited := range []bool{true, false} {
		name := "limited restart"
		if !limited {
			name = "unlimited restart"
		}
		t.Run(name, func(t *testing.T) {
			var callbacks []func()
			var timers []*time.Timer
			afterFunc = func(_ time.Duration, callback func()) *time.Timer {
				callbacks = append(callbacks, callback)
				timer := time.NewTimer(math.MaxInt64)
				timers = append(timers, timer)
				return timer
			}
			t.Cleanup(func() { afterFunc = time.AfterFunc })
			t.Setenv(fakeFFmpegFailWriteEnv, "1")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(executable, t.TempDir(), 60, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg := localOnDemandRecorder(t, "duration")
			cfg.Enabled = true
			if err := m.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Stop() })
			r := m.recorders[cfg.ID]
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			deadline := time.Now().Add(3 * time.Second)
			for r.Status().State == types.ProcessRunning && time.Now().Before(deadline) {
				r.WriteAudio(make([]byte, 16384))
				time.Sleep(5 * time.Millisecond)
			}
			waitForRecorderState(t, r, types.ProcessError)
			// The failed run's deadline must not clear its error.
			callbacks[0]()
			if status := r.Status(); status.State != types.ProcessError || status.Error == "" {
				t.Fatalf("old duration callback cleared the write error: %+v", status)
			}
			t.Setenv(fakeFFmpegFailWriteEnv, "")
			if !limited {
				m.SetMaxDurationMinutes(0)
			}
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			callbacks[0]() // Already queued callbacks can run even after Timer.Stop.
			if status := r.Status(); status.State != types.ProcessRunning {
				t.Fatalf("old duration callback stopped restarted recording: %+v", status)
			}
			if timers[0].Stop() {
				t.Error("old duration timer was still armed")
			}
			if limited {
				callbacks[len(callbacks)-1]()
				waitForRecorderState(t, r, types.ProcessStopped)
			}
		})
	}
}
