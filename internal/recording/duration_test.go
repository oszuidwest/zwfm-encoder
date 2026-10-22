package recording

import (
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
	r := newTestRecorder(t, GenericRecorderConfig{
		Recorder: cfg, FFmpegPath: executable, SpoolDir: t.TempDir(), MaxDurationMinutes: 60,
	})
	t.Cleanup(func() { r.Stop() })
	startCaptureTestRecorder(t, r)
	oldRotation := capturedTimers()[0].callback
	ManualTimers(t)
	cfg.RecordingMode = types.RecordingOnDemand
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	beforeMode := r.currentSegment()
	oldRotation() // A callback already queued before the mode change.
	r.settle()
	if r.currentSegment() != beforeMode {
		t.Fatal("old hourly timer rotated on-demand recording")
	}
	if len(capturedTimers()) != 1 || capturedTimers()[0].delay != time.Hour {
		t.Fatalf("on-demand timers = %v, want one 60-minute limit", capturedTimers())
	}
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(capturedTimers()) != 1 {
		t.Fatal("an edit without a mode change extended the duration limit")
	}
	cfg.RecordingMode = types.RecordingHourly
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(capturedTimers()) != 2 {
		t.Fatal("returning to hourly mode did not schedule a rotation")
	}
	capturedTimers()[0].callback() // A duration callback already queued before the mode change.
	r.settle()
	if r.Status().State != types.ProcessRunning {
		t.Fatal("the old duration limit stopped hourly recording")
	}
	before := r.currentFile()
	capturedTimers()[1].callback()
	r.settle()
	if r.currentFile() == before || len(capturedTimers()) != 3 {
		t.Fatal("the hourly callback did not rotate and schedule the next file")
	}
	r.Stop()
	cfg.RecordingMode = types.RecordingOnDemand
	if err := r.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if len(capturedTimers()) != 3 {
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
			t.Setenv(fakeFFmpegFailWriteEnv, "1")
			cfg := localOnDemandRecorder(t, "duration")
			m, r := startedTestManager(t, cfg)
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			writeUntilRecorderLeavesRunning(t, r)
			waitForRecorderState(t, r, types.ProcessError)
			// The failed run's deadline must not clear its error.
			capturedTimers()[0].callback()
			r.settle()
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
			capturedTimers()[0].callback() // Already queued callbacks can run even after Timer.Stop.
			r.settle()
			if status := r.Status(); status.State != types.ProcessRunning {
				t.Fatalf("old duration callback stopped restarted recording: %+v", status)
			}
			if capturedTimers()[0].timer.Stop() {
				t.Error("old duration timer was still armed")
			}
			if limited {
				capturedTimers()[len(capturedTimers())-1].callback()
				waitForRecorderState(t, r, types.ProcessStopped)
			}
		})
	}
}
