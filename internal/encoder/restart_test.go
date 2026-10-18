package encoder

import (
	"context"
	"errors"
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
