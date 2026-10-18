package recording

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

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
			if err := m.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			r := m.recorders[cfg.ID]
			t.Cleanup(func() { _ = r.Stop() })
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
			t.Setenv(fakeFFmpegFailWriteEnv, "")
			if !limited {
				m.SetMaxDurationMinutes(0)
			}
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			r.mu.RLock()
			file := r.currentFile
			r.mu.RUnlock()
			callbacks[0]() // Already queued callbacks can run even after Timer.Stop.
			if status := r.Status(); status.State != types.ProcessRunning {
				t.Fatalf("old duration callback stopped restarted recording: %+v", status)
			}
			if timers[0].Stop() {
				t.Error("old duration timer was still armed")
			}
			r.mu.RLock()
			if r.currentFile != file {
				t.Error("stale callback changed the current file")
			}
			r.mu.RUnlock()
			if limited {
				callbacks[len(callbacks)-1]()
				waitForRecorderState(t, r, types.ProcessStopped)
			}
		})
	}
}
