package encoder

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// startDanteEncoder starts an encoder in a synctest bubble. Its first source
// finishes teardown when firstDone closes; later sources finish on cancel.
func startDanteEncoder(t *testing.T, firstDone func(context.Context) <-chan struct{}) (*Encoder, *atomic.Int32) {
	t.Helper()
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	e.config.Audio.Input = "dante://tx/Left/Right"
	opens := new(atomic.Int32)
	e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
		done := ctx.Done()
		if opens.Add(1) == 1 {
			done = firstDone(ctx)
		}
		return pipeDanteSource(ctx, done), nil
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	return e, opens
}

// slowTeardown finishes a source's teardown delay after its cancellation.
func slowTeardown(delay time.Duration) func(context.Context) <-chan struct{} {
	return func(ctx context.Context) <-chan struct{} {
		done := make(chan struct{})
		go func() { <-ctx.Done(); time.Sleep(delay); close(done) }()
		return done
	}
}

// heldTeardown finishes teardown only when release closes.
func heldTeardown(release chan struct{}) func(context.Context) <-chan struct{} {
	return func(context.Context) <-chan struct{} { return release }
}

func TestRestartWaitsForActualSourceTeardown(t *testing.T) {
	for _, delay := range []time.Duration{sourceShutdownTimeout - 50*time.Millisecond, sourceShutdownTimeout + 50*time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, opens := startDanteEncoder(t, slowTeardown(delay))
				started := time.Now()
				if err := e.Restart(); err != nil {
					t.Fatalf("Restart after slow teardown: %v", err)
				}
				synctest.Wait()
				if e.State() != types.StateRunning || opens.Load() != 2 || time.Since(started) < delay {
					t.Fatalf("state=%s, opens=%d, elapsed=%s; want one replacement after teardown", e.State(), opens.Load(), time.Since(started))
				}
			})
		})
	}
}

func TestStopWaitsOutCaptureForcedKill(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "ignore-stop")
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, "capture installed its signal handler", func() bool {
		_, last := e.audioTimes(1)
		return !last.IsZero()
	})
	started := time.Now()
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop during forced kill: %v", err)
	}
	if time.Since(started) < types.ShutdownTimeout {
		t.Fatal("test capture exited without exercising the full forced-kill delay")
	}
}

func TestRestartWaitsForAnAlreadyRunningStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, opens := startDanteEncoder(t, slowTeardown(types.ShutdownTimeout+50*time.Millisecond))
		stopped := make(chan error, 1)
		go func() { stopped <- e.Stop() }()
		synctest.Wait()
		if err := e.Restart(); err != nil {
			t.Fatalf("Restart during Stop: %v", err)
		}
		<-stopped
		synctest.Wait()
		if e.State() != types.StateRunning || opens.Load() != 2 {
			t.Fatalf("state=%s, opens=%d; want one replacement", e.State(), opens.Load())
		}
	})
}

func TestRestartFailureIsPublishedToStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newSourceLifecycleTestEncoder(t, "exit")
		t.Cleanup(e.alertOrchestrator.Close)
		e.ffmpegPath = ""
		if err := e.Restart(); !errors.Is(err, ErrFFmpegNotAvailable) {
			t.Fatalf("Restart = %v, want missing FFmpeg", err)
		}
		if got := e.Status().LastError; !strings.Contains(got, "restart") || !strings.Contains(got, "ffmpeg") {
			t.Fatalf("operator-visible error = %q, want failed restart reason", got)
		}
	})
}

func TestExplicitStopSupersedesRestartsWaitingForTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e, opens := startDanteEncoder(t, heldTeardown(release))
		first, queued, stopped := make(chan error, 1), make(chan error, 1), make(chan error, 1)
		go func() { first <- e.Restart() }()
		synctest.Wait()
		go func() { queued <- e.Restart() }()
		synctest.Wait()
		go func() { stopped <- e.Stop() }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop during a stop returned before the source finished its teardown")
		default:
		}
		close(release)
		<-stopped
		<-first
		<-queued
		synctest.Wait()
		if e.State() != types.StateStopped || opens.Load() != 1 {
			t.Fatalf("state=%s, opens=%d; explicit Stop must supersede both pending restarts", e.State(), opens.Load())
		}
	})
}

func TestRestartDeadlineDoesNotStartOverAnUnfinishedSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e, _ := startDanteEncoder(t, heldTeardown(release))
		started := time.Now()
		err := e.Restart()
		close(release)
		if err == nil || time.Since(started) != sourceShutdownTimeout+restartTimeout {
			t.Fatalf("Restart = %v after %s, want teardown deadline", err, time.Since(started))
		}
		if !strings.Contains(e.Status().LastError, "restart") {
			t.Fatal("restart timeout was not published to operator status")
		}
		synctest.Wait()
		if e.State() != types.StateStopped || e.sourceRunID != 1 {
			t.Fatal("restart deadline overlapped source runs")
		}
	})
}

func TestConcurrentRestartsBothSucceed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := startDanteEncoder(t, slowTeardown(time.Second))
		results := make(chan error, 2)
		go func() { results <- e.Restart() }()
		go func() { results <- e.Restart() }()
		err := errors.Join(<-results, <-results)
		synctest.Wait()
		if err != nil || e.State() != types.StateRunning {
			t.Fatalf("err=%v state=%s; want two serialized restarts", err, e.State())
		}
	})
}

func TestStopAfterBackgroundRestartRequestWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, opens := startDanteEncoder(t, slowTeardown(0))
		// The restart goroutine has not run yet when the operator stops.
		e.RestartInBackground()
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if e.State() != types.StateStopped || opens.Load() != 1 {
			t.Fatalf("state=%s, opens=%d; a Stop after the restart request must win", e.State(), opens.Load())
		}
	})
}

// countingLog counts records with one message.
type countingLog struct {
	slog.Handler
	message string
	count   atomic.Int32
}

//nolint:gocritic // slog.Handler requires the record to be passed by value.
func (h *countingLog) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.message {
		h.count.Add(1)
	}
	return h.Handler.Handle(ctx, record)
}

func TestBackgroundRestartRequestsCollapseIntoOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e, _ := startDanteEncoder(t, heldTeardown(release))
		stops := &countingLog{Handler: slog.NewTextHandler(io.Discard, nil), message: "source capture stopped gracefully"}
		previous, previousOutput, previousFlags := slog.Default(), log.Writer(), log.Flags()
		slog.SetDefault(slog.New(stops))
		defer func() {
			slog.SetDefault(previous)
			log.SetOutput(previousOutput)
			log.SetFlags(previousFlags)
		}()
		// Hold the gate, so all requests arrive before the first restart stops it.
		e.mu.Lock()
		if e.restartGate == nil {
			e.restartGate = make(chan struct{}, 1)
		}
		gate := e.restartGate
		e.mu.Unlock()
		gate <- struct{}{}
		for range 5 {
			e.RestartInBackground()
		}
		<-gate
		close(release)
		time.Sleep(time.Minute) // Fake time: lets every background restart finish.
		synctest.Wait()
		// The first restart and one more for the requests queued behind it.
		if e.State() != types.StateRunning || stops.count.Load() != 2 {
			t.Fatalf("state=%s, stop cycles=%d; want queued requests to collapse into one restart", e.State(), stops.count.Load())
		}
	})
}

func TestSlowRestartIsReportedWhileItWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e, _ := startDanteEncoder(t, heldTeardown(release))
		done := make(chan error, 1)
		go func() { done <- e.Restart() }()
		time.Sleep(restartTimeout + time.Millisecond)
		synctest.Wait()
		if got := e.Status().LastError; !strings.Contains(got, "still waiting for the previous input") {
			t.Fatalf("last error while the restart waits = %q, want it to say so", got)
		}
		close(release)
		<-done
	})
}

// R1: restarts, also a failed one, leave recordings alone; Stop ends them.
func TestRestartsKeepRecordingsRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := startDanteEncoder(t, slowTeardown(0))
		probe := &types.Recorder{
			ID: "probe", Name: "probe", Enabled: true, Codec: types.CodecPCM,
			RecordingMode: types.RecordingOnDemand, StorageMode: types.StorageLocal, LocalPath: t.TempDir(),
		}
		if err := e.recordingManager.AddRecorder(probe); err != nil {
			t.Fatal(err)
		}
		if err := e.recordingManager.StartRecorder(probe.ID); err != nil {
			t.Fatal(err)
		}
		state := func() types.ProcessState { synctest.Wait(); return e.recordingManager.Statuses()[probe.ID].State }
		// Without FFmpeg the run fails, and stays failed until something stops it.
		if got := state(); got != types.ProcessError {
			t.Fatalf("probe state = %s, want error", got)
		}
		if err := e.Restart(); err != nil {
			t.Fatal(err)
		}
		e.config.Audio.Input = ""
		if err := e.Restart(); !errors.Is(err, ErrNoAudioInput) {
			t.Fatalf("Restart without input = %v, want ErrNoAudioInput", err)
		}
		if got := state(); got != types.ProcessError {
			t.Fatalf("probe state after restarts = %s; a restart stopped the recordings", got)
		}
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != types.ProcessStopped {
			t.Fatalf("probe state after Stop = %s, want stopped", got)
		}
	})
}

func TestInputChangeStartsEncoderLeftStoppedByAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := startDanteEncoder(t, slowTeardown(0))
		e.config.Audio.Input = ""
		if err := e.Restart(); !errors.Is(err, ErrNoAudioInput) {
			t.Fatalf("Restart without input = %v, want ErrNoAudioInput", err)
		}
		e.config.Audio.Input = "dante://tx/Left/Right"
		e.RestartInBackground()
		time.Sleep(time.Minute)
		synctest.Wait()
		if e.State() != types.StateRunning {
			t.Fatalf("state=%s; a working input must start an encoder a failure stopped", e.State())
		}
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
		e.RestartInBackground()
		time.Sleep(time.Minute)
		synctest.Wait()
		if e.State() != types.StateStopped {
			t.Fatal("an input change started an encoder the operator stopped")
		}
	})
}

func TestInputChangeRecoversAfterExplicitStopAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, opens := startDanteEncoder(t, slowTeardown(0))
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := e.Restart(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if e.State() != types.StateRunning || opens.Load() != 2 {
			t.Fatalf("state=%s, opens=%d; want a running replacement", e.State(), opens.Load())
		}

		e.config.Audio.Input = ""
		if err := e.Restart(); !errors.Is(err, ErrNoAudioInput) {
			t.Fatalf("Restart without input = %v, want ErrNoAudioInput", err)
		}
		e.config.Audio.Input = "dante://tx/Left/Right"
		e.RestartInBackground()
		synctest.Wait()
		if e.State() != types.StateRunning || opens.Load() != 3 {
			t.Fatalf("state=%s, opens=%d; a working input must recover after the failed restart", e.State(), opens.Load())
		}
	})
}

func TestStaleRestartLeavesOperatorRunAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, opens := startDanteEncoder(t, slowTeardown(0))
		e.mu.RLock()
		stale := e.lifecycleGeneration
		e.mu.RUnlock()
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := e.restart(stale); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if e.State() != types.StateRunning || opens.Load() != 2 {
			t.Fatalf("state=%s, opens=%d; a stale restart must not stop the operator's run", e.State(), opens.Load())
		}
	})
}

func TestStallNoticeEndsWithTheRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e, opens := startDanteEncoder(t, heldTeardown(release))
		done := make(chan error, 1)
		go func() { done <- e.Restart() }()
		time.Sleep(restartTimeout + time.Millisecond)
		synctest.Wait()
		// An explicit Stop supersedes the restart; its notice must not stay behind.
		stopped := make(chan error, 1)
		go func() { stopped <- e.Stop() }()
		synctest.Wait() // Stop has invalidated the restart before teardown ends.
		close(release)
		<-done
		<-stopped
		synctest.Wait()
		if got := e.Status().LastError; got != "" || opens.Load() != 1 {
			t.Fatalf("after a superseded restart: last error %q, source opens %d; want none and 1", got, opens.Load())
		}
	})
}
