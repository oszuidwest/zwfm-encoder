package recording

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestStaleStartValidationCannotChangeStoppedOrNewRun(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "stopped"
		if restart {
			name = "new run"
		}
		t.Run(name, func(t *testing.T) {
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseOld := sync.OnceFunc(func() { close(release) })
			original := checkRecordingPath
			var calls atomic.Int32
			checkRecordingPath = func(path string) error {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					return errors.New("old mount failed")
				}
				return original(path)
			}
			cfg := localOnDemandRecorder(t, "validation")
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
			r.state, r.startGen = types.ProcessStarting, 1
			go func() { r.startAsync(1); close(returned) }()
			t.Cleanup(func() { _ = r.Stop(); releaseOld(); <-returned; checkRecordingPath = original })
			<-entered
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			want := types.ProcessStopped
			if restart {
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				want = types.ProcessRunning
				waitForRecorderState(t, r, want)
			}
			releaseOld()
			<-returned
			if got := r.Status(); got.State != want || got.Error != "" {
				t.Fatalf("old validation changed current status: %+v, want %s without error", got, want)
			}
		})
	}
}

func TestStoppedManagerDoesNotRetryHourlyRecorder(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	original := checkRecordingPath
	checkRecordingPath = func(string) error { entered <- struct{}{}; <-release; return errors.New("held mount") }
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := localOnDemandRecorder(t, "retry")
	cfg.RecordingMode, cfg.Enabled = types.RecordingHourly, true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	t.Cleanup(func() { _ = m.Stop(); close(release); checkRecordingPath = original })
	r.mu.Lock()
	r.state = types.ProcessError
	r.mu.Unlock()
	m.retryFailedHourlyRecorders()
	select {
	case <-entered:
		t.Fatal("stopped manager retried an hourly recorder")
	case <-time.After(30 * time.Millisecond):
	}
	if got := r.Status().State; got != types.ProcessError {
		t.Fatalf("state = %s, want original error", got)
	}
}

func TestRunningManagerRetriesFailedHourlyRecorder(t *testing.T) {
	neverRotate(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(executable, t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	cfg := localOnDemandRecorder(t, "retry-active")
	cfg.RecordingMode = types.RecordingHourly
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	r.mu.Lock()
	r.config.Enabled, r.state = true, types.ProcessError
	r.mu.Unlock()
	m.retryFailedHourlyRecorders()
	waitForRecorderState(t, r, types.ProcessRunning)
}

func TestManagerStartWaitsForPreviousStop(t *testing.T) {
	testManagerStartWaitsForPreviousStop(t, false)
}

func TestManagerGuardedStartWaitsForPreviousStop(t *testing.T) {
	testManagerStartWaitsForPreviousStop(t, true)
}

func testManagerStartWaitsForPreviousStop(t *testing.T, guarded bool) {
	t.Helper()
	neverRotate(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(executable, t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := m.Start
	if guarded {
		start = func() error { return m.StartIfActive(func() bool { return true }) }
	}
	cfg := localOnDemandRecorder(t, "retiring")
	cfg.RecordingMode, cfg.Enabled = types.RecordingHourly, true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	waitForRecorderState(t, r, types.ProcessRunning)
	r.retiring.Add(1) // A previous file is still being finalized during Stop.
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop() }()
	waitForRecorderState(t, r, types.ProcessStopping)
	started := make(chan error, 1)
	go func() { started <- start() }()
	select {
	case err := <-started:
		r.retiring.Done()
		<-stopped
		_ = m.Stop()
		t.Fatalf("Start completed before previous Stop finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	r.retiring.Done()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Stop() }()
	if !m.IsRunning() {
		t.Fatal("new manager run was lost during old teardown")
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}
