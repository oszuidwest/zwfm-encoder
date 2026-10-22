package recording

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
			entered, release := make(chan struct{}), make(chan struct{})
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
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseOld(); checkRecordingPath = original })
			<-entered
			r.Stop()
			want := types.ProcessStopped
			if restart {
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				// The same directory is still being checked: the new run waits for it.
				if got := r.Status().State; got != types.ProcessStarting {
					t.Fatalf("restart state = %s while the old check runs, want starting", got)
				}
				want = types.ProcessRunning
			}
			releaseOld()
			if restart {
				waitForRecorderState(t, r, want)
			} else {
				time.Sleep(50 * time.Millisecond) // Lets the old check report back.
			}
			if got := r.Status(); got.State != want || got.Error != "" {
				t.Fatalf("old validation changed current status: %+v, want %s without error", got, want)
			}
		})
	}
}

// TestFailedHourlyRecorderRetriesAtNextHour fails the start of an hourly
// recorder and fires the retry its failure armed.
func TestFailedHourlyRecorderRetriesAtNextHour(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manager, *types.Recorder) error
		want   types.ProcessState
	}{
		{"running", nil, types.ProcessRunning},
		{"manager stopped", func(m *Manager, _ *types.Recorder) error { return m.Stop() }, types.ProcessStopped},
		{"disabled", func(m *Manager, cfg *types.Recorder) error {
			cfg.Enabled = false
			return m.UpdateRecorder(cfg)
		}, types.ProcessStopped},
		{"switched to on-demand", func(m *Manager, cfg *types.Recorder) error {
			cfg.RecordingMode = types.RecordingOnDemand
			return m.UpdateRecorder(cfg)
		}, types.ProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "retry")
			cfg.RecordingMode = types.RecordingHourly
			m, r := newTestManager(t, cfg)
			r.ffmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			r.settle()
			if got := r.Status().State; got != types.ProcessError {
				t.Fatalf("start with a missing FFmpeg: state %s, want error", got)
			}
			r.ffmpegPath = os.Args[0]
			if tc.change != nil {
				saved := r.Config()
				if err := tc.change(m, &saved); err != nil {
					t.Fatal(err)
				}
			}
			retry := capturedTimers()[0].callback
			retry() // Also when it was already due before the change.
			r.settle()
			if got := r.Status().State; got != tc.want {
				t.Fatalf("after the hourly retry: state %s, want %s", got, tc.want)
			}
		})
	}
}

func TestManagerStartWaitsForPreviousStop(t *testing.T) {
	cfg := localOnDemandRecorder(t, "retiring")
	cfg.RecordingMode = types.RecordingHourly
	m, r := startedTestManager(t, cfg)
	waitForRecorderState(t, r, types.ProcessRunning)
	release := holdTeardown(t, r) // A previous file is still being finalized during Stop.
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop() }()
	waitForRecorderState(t, r, types.ProcessStopping)
	started := make(chan error, 1)
	go func() { started <- m.Start() }()
	select {
	case err := <-started:
		release()
		<-stopped
		t.Fatalf("Start completed before previous Stop finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if !m.IsRunning() {
		t.Fatal("new manager run was lost during old teardown")
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}

// newTestManager returns a manager with cfg, recording through the fake FFmpeg,
// which finalizes at once.
func newTestManager(t *testing.T, cfg *types.Recorder) (*Manager, *GenericRecorder) {
	t.Helper()
	neverRotate(t)
	t.Setenv(fakeFFmpegFinalizeEnv, "0s")
	// os.Executable, not os.Args: gosec taints the args into the spool paths.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(executable, t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(); m.Close() })
	return m, m.recorders[cfg.ID]
}

// startedTestManager returns a running manager with cfg.
func startedTestManager(t *testing.T, cfg *types.Recorder) (*Manager, *GenericRecorder) {
	t.Helper()
	m, r := newTestManager(t, cfg)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	return m, r
}

// holdTeardown makes r's next Stop wait as if a previous file were still being
// finalized, until the returned function or the test cleanup releases it.
func holdTeardown(t *testing.T, r *GenericRecorder) (release func()) {
	t.Helper()
	r.inspect(func(l *run) { l.retiring++ })
	release = sync.OnceFunc(func() {
		r.inspect(func(l *run) {
			l.retiring--
			r.finishStop()
		})
	})
	t.Cleanup(release)
	return release
}

func TestUpdateStartsRecorderThatBecomesEnabledHourly(t *testing.T) {
	cfg := localOnDemandRecorder(t, "reconcile")
	m, r := startedTestManager(t, cfg)
	updated := r.Config()
	updated.RecordingMode = types.RecordingHourly
	if err := m.UpdateRecorder(&updated); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}

func TestUpdateStopsDisabledRecorder(t *testing.T) {
	cfg := localOnDemandRecorder(t, "disable")
	m, r := startedTestManager(t, cfg)
	if err := m.StartRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	disabled := r.Config()
	disabled.Enabled = false
	if err := m.UpdateRecorder(&disabled); err != nil {
		t.Fatal(err)
	}
	if got := r.Status().State; got != types.ProcessStopped {
		t.Fatalf("disabling save returned in state %s, want stopped", got)
	}
}

func TestUpdateDoesNotStartRecorderAfterManagerStop(t *testing.T) {
	cfg := localOnDemandRecorder(t, "stopped")
	cfg.RecordingMode = types.RecordingHourly
	m, r := startedTestManager(t, cfg)
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	enabled := r.Config()
	if err := m.UpdateRecorder(&enabled); err != nil {
		t.Fatal(err)
	}
	if got := r.Status().State; got != types.ProcessStopped {
		t.Fatalf("update started a recorder after Manager.Stop: state = %s", got)
	}
	// The enable still takes effect at the next Start.
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}

func TestStartRecorderRefusesStart(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		disabled, hourly, stopFirst bool
		want                        error
	}{
		// The API's encoder state check can pass just before the encoder stops.
		{name: "after manager stop", stopFirst: true, want: ErrManagerNotRunning},
		{name: "disabled", disabled: true, want: ErrRecorderDisabled},
		// Disabled, so the manager does not start it; the mode is checked first.
		{name: "hourly", disabled: true, hourly: true, want: ErrHourlyRecorderNotControllable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "ondemand")
			cfg.Enabled = !tc.disabled
			if tc.hourly {
				cfg.RecordingMode = types.RecordingHourly
			}
			m, r := startedTestManager(t, cfg)
			if tc.stopFirst {
				if err := m.Stop(); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.StartRecorder(cfg.ID); !errors.Is(err, tc.want) {
				t.Fatalf("StartRecorder = %v, want %v", err, tc.want)
			}
			if got := r.Status().State; got != types.ProcessStopped {
				t.Fatalf("recorder started: state = %s", got)
			}
		})
	}
}

// TestUpdateDuringStopAppliesAfterIt saves a recorder while a stop is still
// finalizing: a switch to enabled hourly starts it once the stop is done, and
// a disabling save waits for that stop.
func TestUpdateDuringStopAppliesAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*Manager, *types.Recorder) error
		save func(*types.Recorder)
		want types.ProcessState
	}{
		{"disabling update, then hourly", func(m *Manager, cfg *types.Recorder) error {
			cfg.Enabled = false
			return m.UpdateRecorder(cfg)
		}, func(cfg *types.Recorder) { cfg.Enabled = true }, types.ProcessRunning},
		{"api stop, then hourly", func(m *Manager, cfg *types.Recorder) error {
			return m.StopRecorder(cfg.ID)
		}, func(cfg *types.Recorder) { cfg.RecordingMode = types.RecordingHourly }, types.ProcessRunning},
		{"api stop, then disabled", func(m *Manager, cfg *types.Recorder) error {
			return m.StopRecorder(cfg.ID)
		}, func(cfg *types.Recorder) { cfg.Enabled = false }, types.ProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "reconcile-stop")
			if tc.name == "disabling update, then hourly" {
				cfg.RecordingMode = types.RecordingHourly
			}
			m, r := startedTestManager(t, cfg)
			if cfg.RecordingMode == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			release := holdTeardown(t, r) // The first Stop waits.
			first, saved := r.Config(), r.Config()
			tc.save(&saved)
			stopped := make(chan error, 1)
			go func() { stopped <- tc.stop(m, &first) }()
			waitForRecorderState(t, r, types.ProcessStopping)
			updated := make(chan error, 1)
			go func() { updated <- m.UpdateRecorder(&saved) }()
			if !saved.Enabled {
				select {
				case err := <-updated:
					t.Fatalf("disabling save returned while the recorder was still stopping: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
			}
			release()
			for _, done := range []chan error{stopped, updated} {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, tc.want)
		})
	}
}

func TestStopJoinsTeardownOfDisabledRecorder(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*Manager, string) error
	}{
		{"manager stop", func(m *Manager, _ string) error { return m.Stop() }},
		{"remove", (*Manager).RemoveRecorder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "join")
			cfg.RecordingMode = types.RecordingHourly
			m, r := startedTestManager(t, cfg)
			waitForRecorderState(t, r, types.ProcessRunning)
			release := holdTeardown(t, r) // The disabling Stop waits.
			disabled := r.Config()
			disabled.Enabled = false
			disabling := make(chan error, 1)
			go func() { disabling <- m.UpdateRecorder(&disabled) }()
			waitForRecorderState(t, r, types.ProcessStopping)
			stopped := make(chan error, 1)
			go func() { stopped <- tc.stop(m, cfg.ID) }()
			select {
			case err := <-stopped:
				// Shutdown could then exit before the recording is finalized.
				t.Fatalf("returned while the disabled recorder was still stopping: %v, state %s", err, r.Status().State)
			case <-time.After(30 * time.Millisecond):
			}
			release()
			for _, done := range []chan error{disabling, stopped} {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "remove" {
				if _, exists := m.Statuses()[cfg.ID]; exists {
					t.Fatal("removed recorder still present")
				}
			}
			if got := r.Status().State; got != types.ProcessStopped {
				t.Fatalf("state after the joined stop = %s, want stopped", got)
			}
		})
	}
}

func TestUpdateTearsDownFailedRecorder(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode types.RecordingMode
		save func(*types.Recorder)
		want types.ProcessState
	}{
		{"disabled", types.RecordingHourly, func(cfg *types.Recorder) { cfg.Enabled = false }, types.ProcessStopped},
		{"on-demand saved", types.RecordingOnDemand, func(*types.Recorder) {}, types.ProcessStopped},
		// The save corrects the config, like an operator fixing the cause.
		{"hourly saved", types.RecordingHourly, func(cfg *types.Recorder) { cfg.LocalPath += "-fixed" }, types.ProcessRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(fakeFFmpegFailWriteEnv, "1")
			cfg := localOnDemandRecorder(t, "failed")
			cfg.RecordingMode = tc.mode
			m, r := startedTestManager(t, cfg)
			if tc.mode == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			writeUntilRecorderLeavesRunning(t, r)
			waitForRecorderState(t, r, types.ProcessError)
			r.settle() // The write-error cleanup has finished.
			t.Setenv(fakeFFmpegFailWriteEnv, "")
			saved := r.Config()
			tc.save(&saved)
			if err := m.UpdateRecorder(&saved); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, tc.want)
			if err := m.Stop(); err != nil {
				t.Fatal(err)
			}
			if state := r.Status().State; state != types.ProcessStopped {
				t.Fatalf("after the save and Manager.Stop: state %s, want stopped", state)
			}
		})
	}
}

// A save that leaves the recorder enabled with the same output settings must
// not cut the running file.
func TestUpdateKeepsRunningRecorderRecording(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to types.RecordingMode
	}{
		{"hourly renamed", types.RecordingHourly, types.RecordingHourly},
		{"on-demand renamed", types.RecordingOnDemand, types.RecordingOnDemand},
		{"hourly to on-demand", types.RecordingHourly, types.RecordingOnDemand},
		{"on-demand to hourly", types.RecordingOnDemand, types.RecordingHourly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "keep")
			cfg.RecordingMode = tc.from
			m, r := startedTestManager(t, cfg)
			if tc.from == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			before := r.currentSegment()
			saved := r.Config()
			saved.Name, saved.RecordingMode = "renamed", tc.to
			if err := m.UpdateRecorder(&saved); err != nil {
				t.Fatal(err)
			}
			r.settle()
			if state, after := r.Status().State, r.currentSegment(); state != types.ProcessRunning || after != before {
				t.Fatalf("save restarted or stopped the recording: state %s, file %v -> %v", state, before.file, after)
			}
		})
	}
}

// Adding an on-demand or disabled recorder to a running manager must not start it.
func TestAddRecorderToRunningManagerStartsOnlyEnabledHourly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    types.RecordingMode
		enabled bool
		want    types.ProcessState
	}{
		{"enabled hourly", types.RecordingHourly, true, types.ProcessRunning},
		{"disabled hourly", types.RecordingHourly, false, types.ProcessStopped},
		{"on-demand", types.RecordingOnDemand, true, types.ProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := localOnDemandRecorder(t, "first")
			m, _ := startedTestManager(t, first)
			cfg := localOnDemandRecorder(t, "added")
			cfg.RecordingMode, cfg.Enabled = tc.mode, tc.enabled
			if err := m.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			r := m.recorders[cfg.ID]
			if tc.want == types.ProcessRunning {
				waitForRecorderState(t, r, tc.want)
			} else if got := r.Status().State; got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

// A close that gives up on a hung finalization still releases everyone
// waiting for that stop.
func TestCloseReleasesStopWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRecorder(t, GenericRecorderConfig{Recorder: localOnDemandRecorder(t, "waiters")})
		r.inspect(func(l *run) { l.state = types.ProcessRunning })
		holdTeardown(t, r) // A finalization that never ends.
		stopped := make(chan struct{})
		go func() { r.Stop(); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("the stop ended before its finalization")
		default:
		}
		r.close() // Gives up after recorderCloseTimeout.
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("a Stop waiting on the removed recorder never returned")
		}
	})
}
