package recording

import (
	"context"
	"errors"
	"log/slog"
	"math"
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
				<-returned
				return original(path)
			}
			cfg := localOnDemandRecorder(t, "validation")
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
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
				// The same directory is still being checked: the new run waits for it.
				if got := r.Status().State; got != types.ProcessStarting {
					t.Fatalf("restart state = %s while the old check runs, want starting", got)
				}
				want = types.ProcessRunning
			}
			releaseOld()
			<-returned
			if restart {
				waitForRecorderState(t, r, want)
			}
			if got := r.Status(); got.State != want || got.Error != "" {
				t.Fatalf("old validation changed current status: %+v, want %s without error", got, want)
			}
		})
	}
}

func TestStoppedManagerDoesNotRetryHourlyRecorder(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	cfg := localOnDemandRecorder(t, "retry")
	cfg.RecordingMode, cfg.Enabled = types.RecordingHourly, true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	r.mu.Lock()
	r.state = types.ProcessError
	r.mu.Unlock()
	m.retryFailedHourlyRecorders() // Start is synchronous up to ProcessStarting.
	if got := r.Status().State; got != types.ProcessError {
		t.Fatalf("stopped manager retried hourly recorder: state = %s", got)
	}
}

// runningManagerWithFailedHourlyRecorder returns a started manager whose
// enabled hourly recorder is in error, so the hourly retry starts it.
func runningManagerWithFailedHourlyRecorder(t *testing.T) (*Manager, *GenericRecorder) {
	t.Helper()
	cfg := localOnDemandRecorder(t, "retry-active")
	cfg.RecordingMode = types.RecordingHourly
	m, r := startedTestManager(t, cfg)
	r.mu.Lock()
	r.config.Enabled, r.state = true, types.ProcessError
	r.mu.Unlock()
	return m, r
}

// pauseHandler blocks the log record with message until release closes.
type pauseHandler struct {
	slog.Handler
	message          string
	entered, release chan struct{}
}

func (pauseHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h pauseHandler) Handle(_ context.Context, record slog.Record) error { //nolint:gocritic // slog.Handler takes the Record by value.
	if record.Message == h.message {
		close(h.entered)
		<-h.release
	}
	return nil
}

func TestStopWaitsForAnHourlyRetryInFlight(t *testing.T) {
	m, r := runningManagerWithFailedHourlyRecorder(t)
	// Hold async startup before filesystem preparation, so Stop invalidates
	// it before the test's temporary directories can be cleaned up.
	startupEntered, releaseStartup := make(chan struct{}), make(chan struct{})
	previousClock := clockSynced
	clockSynced = func() bool { close(startupEntered); <-releaseStartup; return true }
	t.Cleanup(func() {
		<-startupEntered
		close(releaseStartup)
		clockSynced = previousClock
	})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	previous := slog.Default()
	slog.SetDefault(slog.New(pauseHandler{slog.DiscardHandler, "retrying failed hourly recorder at hour boundary", entered, release}))
	t.Cleanup(func() { unblock(); slog.SetDefault(previous) })
	retried, stopped := make(chan struct{}), make(chan error, 1)
	go func() { m.retryFailedHourlyRecorders(); close(retried) }()
	<-entered // The retry passed its running check and is about to start the recorder.
	go func() { stopped <- m.Stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while the retry was between its check and Start: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	<-retried
	if err := <-stopped; err != nil || r.Status().State != types.ProcessStopped {
		t.Fatalf("after retry and Stop: %v, state %s; want stopped", err, r.Status().State)
	}
}

func TestRunningManagerRetriesFailedHourlyRecorder(t *testing.T) {
	m, r := runningManagerWithFailedHourlyRecorder(t)
	m.retryFailedHourlyRecorders()
	if got := r.Status().State; got == types.ProcessError {
		t.Fatal("retry did not start the recorder before returning")
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}

func TestManagerStartWaitsForPreviousStop(t *testing.T) {
	neverRotate(t)
	// os.Executable, not os.Args: gosec taints the args into the spool paths.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(executable, t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
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
	go func() { started <- m.StartIfActive(func() bool { return true }) }()
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
	return m, m.recorders[cfg.ID]
}

// startedTestManager returns a running manager with cfg.
func startedTestManager(t *testing.T, cfg *types.Recorder) (*Manager, *GenericRecorder) {
	t.Helper()
	m, r := newTestManager(t, cfg)
	t.Cleanup(func() { _ = m.Stop() })
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	return m, r
}

// managerRunningWithoutSchedulers returns a manager that is running for
// recorder control, without the background goroutines that take its locks.
func managerRunningWithoutSchedulers(t *testing.T, cfg *types.Recorder) (*Manager, *GenericRecorder) {
	t.Helper()
	m, r := newTestManager(t, cfg)
	m.running = true
	t.Cleanup(func() { _ = r.Stop() })
	return m, r
}

// holdTeardown makes r's next Stop wait as if a previous file were still being
// finalized, until the returned function or the test cleanup releases it.
func holdTeardown(t *testing.T, r *GenericRecorder) (release func()) {
	t.Helper()
	r.retiring.Add(1)
	release = sync.OnceFunc(r.retiring.Done)
	t.Cleanup(release)
	return release
}

// waitForPendingWriter returns once a writer waits for mu, which the caller
// holds for reading.
func waitForPendingWriter(t *testing.T, mu *sync.RWMutex) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mu.TryRLock() {
		mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("no writer waited for the lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUpdateStartsRecorderThatBecomesEnabledHourly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   types.RecordingMode
		enable bool
	}{
		{name: "enabled", from: types.RecordingHourly, enable: true},
		{name: "switched to hourly", from: types.RecordingOnDemand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "reconcile")
			cfg.RecordingMode, cfg.Enabled = tc.from, !tc.enable
			m, r := startedTestManager(t, cfg)
			if got := r.Status().State; got != types.ProcessStopped {
				t.Fatalf("setup: state = %s, want stopped", got)
			}
			updated := r.Config()
			updated.RecordingMode, updated.Enabled = types.RecordingHourly, true
			if err := m.UpdateRecorder(&updated); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
		})
	}
}

func TestUpdateStopsDisabledRecorder(t *testing.T) {
	for _, mode := range []types.RecordingMode{types.RecordingHourly, types.RecordingOnDemand} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "disable")
			cfg.RecordingMode, cfg.Enabled = mode, true
			m, r := startedTestManager(t, cfg)
			if mode == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			disabled := r.Config()
			disabled.Enabled = false
			if err := m.UpdateRecorder(&disabled); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessStopped)
		})
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
	enabled.Enabled = true
	if err := m.UpdateRecorder(&enabled); err != nil {
		t.Fatal(err)
	}
	// Start is synchronous up to ProcessStarting.
	if got := r.Status().State; got != types.ProcessStopped {
		t.Fatalf("update started a recorder after Manager.Stop: state = %s", got)
	}
	// The enable still takes effect at the next Start.
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
}

func TestUpdateDoesNotStartRemovedRecorder(t *testing.T) {
	cfg := localOnDemandRecorder(t, "removed")
	cfg.RecordingMode = types.RecordingHourly
	m, r := managerRunningWithoutSchedulers(t, cfg)
	enabled := r.Config()
	enabled.Enabled = true
	r.mu.RLock() // Hold the update in UpdateConfig, after its lookup.
	unlock := sync.OnceFunc(r.mu.RUnlock)
	t.Cleanup(unlock) // Runs before the recorder cleanup, also after a failure.
	updated := make(chan error, 1)
	go func() { updated <- m.UpdateRecorder(&enabled) }()
	waitForPendingWriter(t, &r.mu)
	// Remove it from the manager, as RemoveRecorder does before stopping it.
	m.mu.Lock()
	delete(m.recorders, cfg.ID)
	m.publishAudioRecordersLocked()
	m.mu.Unlock()
	unlock()
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if got := r.Status().State; got != types.ProcessStopped {
		t.Fatalf("update started a removed recorder: state = %s", got)
	}
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

func TestStartRecorderHoldsStopLockUntilStarted(t *testing.T) {
	cfg := localOnDemandRecorder(t, "ondemand")
	cfg.Enabled = true
	m, r := managerRunningWithoutSchedulers(t, cfg)
	r.mu.RLock() // Hold the start where it takes the recorder lock.
	unlock := sync.OnceFunc(r.mu.RUnlock)
	t.Cleanup(unlock) // Runs before the recorder cleanup, also after a failure.
	started := make(chan error, 1)
	go func() { started <- m.StartRecorder(cfg.ID) }()
	waitForPendingWriter(t, &r.mu)
	// Otherwise Stop can pass between the running check and Start and return
	// before the recorder starts.
	if m.mu.TryLock() {
		m.mu.Unlock()
		t.Error("StartRecorder released the lock Stop takes before starting")
	}
	unlock()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	// Stop does not wait for a start still preparing its file.
	waitForRecorderState(t, r, types.ProcessRunning)
}

// TestUpdateWaitsForReconcilingStop checks that an update switching the
// recorder to enabled hourly waits for a stop that an earlier update or the
// API is still making, and then starts the recorder.
func TestUpdateWaitsForReconcilingStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode types.RecordingMode
		stop func(*Manager, *types.Recorder) error
	}{
		{"disabling update", types.RecordingHourly, func(m *Manager, cfg *types.Recorder) error {
			cfg.Enabled = false
			return m.UpdateRecorder(cfg)
		}},
		// A stale API stop that checked the recorder before the update could
		// otherwise stop it after the update started it as hourly.
		{"api stop", types.RecordingOnDemand, func(m *Manager, cfg *types.Recorder) error {
			return m.StopRecorder(cfg.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "reconcile-stop")
			cfg.RecordingMode, cfg.Enabled = tc.mode, true
			m, r := startedTestManager(t, cfg)
			if tc.mode == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			joined, release := observeStopJoin(t, holdTeardown(t, r)) // The first Stop waits.
			waiting := make(chan struct{})
			markWaiting := sync.OnceFunc(func() { close(waiting) })
			previous := lockReconcile
			lockReconcile = func(mu *sync.Mutex) {
				if !mu.TryLock() {
					markWaiting()
					previous(mu)
				}
			}
			t.Cleanup(func() { lockReconcile = previous })
			hourly, first := r.Config(), r.Config()
			hourly.RecordingMode = types.RecordingHourly
			stopped := make(chan error, 1)
			go func() { stopped <- tc.stop(m, &first) }()
			waitForRecorderState(t, r, types.ProcessStopping)
			updated := make(chan error, 1)
			go func() { updated <- m.UpdateRecorder(&hourly) }()
			select {
			case err := <-updated:
				// Its start was refused while stopping, and the hourly retry only retries errors.
				t.Fatalf("update returned while the %s was still stopping the recorder: %v, state %s", tc.name, err, r.Status().State)
			case <-joined:
				// Joining alone is not enough: a later update's config change must not
				// interleave with an earlier stop or start of the same recorder.
				t.Fatalf("update overlapped the %s instead of waiting for it", tc.name)
			case <-waiting:
			}
			release()
			for _, done := range []chan error{stopped, updated} {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
		})
	}
}

// observeStopJoin reports through joined when a caller joins a Stop in
// progress, and fails the test if that caller waits on a finished stop or its
// wait ends before release lets the teardown finish.
func observeStopJoin(t *testing.T, finish func()) (joined <-chan struct{}, release func()) {
	t.Helper()
	ch := make(chan struct{})
	markJoined := sync.OnceFunc(func() { close(ch) })
	var released, early atomic.Bool
	previous := awaitStop
	awaitStop = func(done <-chan struct{}) {
		// Checked before joined is reported, so the teardown is still held.
		select {
		case <-done:
			early.Store(true)
		default:
		}
		markJoined()
		previous(done)
		if !released.Load() {
			early.Store(true)
		}
	}
	t.Cleanup(func() {
		awaitStop = previous
		if early.Load() {
			t.Error("a joined Stop returned before the teardown it joined was released")
		}
	})
	return ch, func() { released.Store(true); finish() }
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
			cfg.RecordingMode, cfg.Enabled = types.RecordingHourly, true
			m, r := startedTestManager(t, cfg)
			waitForRecorderState(t, r, types.ProcessRunning)
			joined, release := observeStopJoin(t, holdTeardown(t, r)) // The disabling Stop waits.
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
			case <-joined:
			}
			release()
			for _, done := range []chan error{disabling, stopped} {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if got := r.Status().State; got != types.ProcessStopped {
				t.Fatalf("state after the joined stop = %s, want stopped", got)
			}
		})
	}
}

func TestHourlyRetryDoesNotStartRecorderChangedMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*types.Recorder)
	}{
		{"disabled", func(cfg *types.Recorder) { cfg.Enabled = false }},
		{"switched to on-demand", func(cfg *types.Recorder) { cfg.RecordingMode = types.RecordingOnDemand }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "retry-changed")
			cfg.RecordingMode, cfg.Enabled = types.RecordingHourly, true
			// The storage monitor would queue a writer behind the paused retry's read
			// lock and so block the update's lookup.
			m, r := managerRunningWithoutSchedulers(t, cfg)
			r.mu.Lock()
			r.state = types.ProcessError
			r.mu.Unlock()
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			previous := slog.Default()
			slog.SetDefault(slog.New(pauseHandler{slog.DiscardHandler, "retrying failed hourly recorder at hour boundary", entered, release}))
			t.Cleanup(func() { unblock(); slog.SetDefault(previous) })
			retried := make(chan struct{})
			go func() { m.retryFailedHourlyRecorders(); close(retried) }()
			<-entered // The retry checked that the recorder is enabled and hourly, and is about to start it.
			changed := r.Config()
			tc.change(&changed)
			if err := m.UpdateRecorder(&changed); err != nil {
				t.Fatal(err)
			}
			unblock()
			<-retried
			// Start is synchronous up to ProcessStarting.
			if got := r.Status().State; got == types.ProcessStarting || got == types.ProcessRunning {
				t.Fatalf("retry started a recorder %s after its check: state = %s", tc.name, got)
			}
		})
	}
}

func TestUpdateJoinsStopStartedElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*types.Recorder)
		want   types.ProcessState
	}{
		{"switched to hourly", func(cfg *types.Recorder) { cfg.RecordingMode = types.RecordingHourly }, types.ProcessRunning},
		{"disabled", func(cfg *types.Recorder) { cfg.Enabled = false }, types.ProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "joined")
			cfg.Enabled = true
			m, r := startedTestManager(t, cfg)
			var timersMu sync.Mutex
			var durationLimits []func()
			previous := afterFunc
			t.Cleanup(func() { afterFunc = previous })
			afterFunc = func(_ time.Duration, callback func()) *time.Timer {
				timersMu.Lock()
				defer timersMu.Unlock()
				durationLimits = append(durationLimits, callback)
				return time.NewTimer(math.MaxInt64)
			}
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			joined, release := observeStopJoin(t, holdTeardown(t, r)) // The duration limit's Stop waits.
			timersMu.Lock()
			limitReached := durationLimits[0]
			timersMu.Unlock()
			limited := make(chan struct{})
			go func() { limitReached(); close(limited) }()
			waitForRecorderState(t, r, types.ProcessStopping)
			changed := r.Config()
			tc.change(&changed)
			updated := make(chan error, 1)
			go func() { updated <- m.UpdateRecorder(&changed) }()
			select {
			case err := <-updated:
				// Switched to hourly: a start refused as stopping is lost. Disabled: the
				// save returns while the recorder is still recording.
				t.Fatalf("update returned while the duration limit's stop was in progress: %v, state %s", err, r.Status().State)
			case <-joined:
			}
			release()
			<-limited
			if err := <-updated; err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, tc.want)
		})
	}
}

func TestUpdateHoldsStopLockUntilStarted(t *testing.T) {
	cfg := localOnDemandRecorder(t, "update-start")
	cfg.RecordingMode = types.RecordingHourly
	m, r := managerRunningWithoutSchedulers(t, cfg)
	entered, release := make(chan struct{}), make(chan struct{})
	markEntered, unblock := sync.OnceFunc(func() { close(entered) }), sync.OnceFunc(func() { close(release) })
	previous := startHourlyRecorder
	startHourlyRecorder = func(r *GenericRecorder) error { markEntered(); <-release; return previous(r) }
	t.Cleanup(func() { unblock(); startHourlyRecorder = previous })
	enabled := r.Config()
	enabled.Enabled = true
	updated := make(chan error, 1)
	go func() { updated <- m.UpdateRecorder(&enabled) }()
	select {
	case err := <-updated:
		t.Fatalf("update returned without starting the enabled hourly recorder: %v", err)
	case <-entered:
	}
	// Manager.Stop and RemoveRecorder take this lock, so neither can finish
	// between the update's running and membership check and its start.
	if m.mu.TryLock() {
		m.mu.Unlock()
		t.Error("UpdateRecorder released the lock Stop and RemoveRecorder take before starting")
	}
	unblock()
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	// Stop does not wait for a start still preparing its file.
	waitForRecorderState(t, r, types.ProcessRunning)
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
			cfg.RecordingMode, cfg.Enabled = tc.mode, true
			m, r := startedTestManager(t, cfg)
			t.Cleanup(func() { _ = r.Stop() })
			if tc.mode == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			writeUntilRecorderLeavesRunning(t, r)
			waitForRecorderState(t, r, types.ProcessError)
			r.retiring.Wait() // The write-error cleanup has finished.
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

// A save that leaves the recorder enabled must not cut the running file.
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
			cfg.RecordingMode, cfg.Enabled = tc.from, true
			m, r := startedTestManager(t, cfg)
			if tc.from == types.RecordingOnDemand {
				if err := m.StartRecorder(cfg.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			r.mu.RLock()
			gen := r.startGen
			r.mu.RUnlock()
			saved := r.Config()
			saved.Name, saved.RecordingMode = "renamed", tc.to
			if err := m.UpdateRecorder(&saved); err != nil {
				t.Fatal(err)
			}
			r.mu.RLock()
			state, after := r.state, r.startGen
			r.mu.RUnlock()
			if state != types.ProcessRunning || after != gen {
				t.Fatalf("save restarted or stopped the recording: state %s, start generation %d -> %d", state, gen, after)
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
			t.Cleanup(func() { _ = r.Stop() })
			if tc.want == types.ProcessRunning {
				waitForRecorderState(t, r, tc.want)
			} else if got := r.Status().State; got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}
