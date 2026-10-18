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

func TestRestartWaitsForActualSourceTeardown(t *testing.T) {
	for _, delay := range []time.Duration{types.ShutdownTimeout + 50*time.Millisecond, types.ShutdownTimeout + 2*time.Second + 50*time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newSourceLifecycleTestEncoder(t, "exit")
				t.Cleanup(e.alertOrchestrator.Close)
				e.config.Audio.Input = "dante://tx/Left/Right"
				var opens atomic.Int32
				e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
					if opens.Add(1) != 1 {
						return pipeDanteSource(ctx, ctx.Done()), nil
					}
					done := make(chan struct{})
					go func() { <-ctx.Done(); time.Sleep(delay); close(done) }()
					return pipeDanteSource(ctx, done), nil
				}
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
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

func TestRestartAfterCaptureNeedsFullKillDelay(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	var launches atomic.Int32
	e.buildCaptureCommand = func(device, path string) (string, []string, error) {
		mode := "pcm"
		if launches.Add(1) == 1 {
			mode = "ignore-stop"
		}
		return helperCaptureCommand(mode)(device, path)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, "capture installed its signal handler", func() bool {
		_, last := e.audioTimes(1)
		return !last.IsZero()
	})
	started := time.Now()
	if err := e.Restart(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, "replacement capture running", func() bool {
		return e.State() == types.StateRunning && launches.Load() == 2
	})
	if time.Since(started) < types.ShutdownTimeout {
		t.Fatal("test capture exited without exercising the full forced-kill delay")
	}
}

func TestRestartWaitsForAnAlreadyRunningStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newSourceLifecycleTestEncoder(t, "exit")
		t.Cleanup(e.alertOrchestrator.Close)
		e.config.Audio.Input = "dante://tx/Left/Right"
		var opens atomic.Int32
		e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
			if opens.Add(1) != 1 {
				return pipeDanteSource(ctx, ctx.Done()), nil
			}
			done := make(chan struct{})
			go func() { <-ctx.Done(); time.Sleep(types.ShutdownTimeout + 50*time.Millisecond); close(done) }()
			return pipeDanteSource(ctx, done), nil
		}
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
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

func TestRestartDeadlineDoesNotStartOverAnUnfinishedSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newSourceLifecycleTestEncoder(t, "exit")
		t.Cleanup(e.alertOrchestrator.Close)
		e.config.Audio.Input = "dante://tx/Left/Right"
		release := make(chan struct{})
		e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
			return pipeDanteSource(ctx, release), nil
		}
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		started := time.Now()
		err := e.Restart()
		close(release)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 30*time.Second {
			t.Fatalf("Restart = %v after %s, want explicit 30s deadline", err, time.Since(started))
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
