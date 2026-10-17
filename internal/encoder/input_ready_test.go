package encoder

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestInputReadinessRequiresRecentPCMFromCurrentRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		state      types.EncoderState
		last, lost time.Time
		audioRun   uint64
		want       bool
	}{
		{name: "opened but no PCM", state: types.StateRunning, audioRun: 1},
		{name: "recent PCM", state: types.StateRunning, audioRun: 1, last: time.Now(), want: true},
		{name: "stalled PCM", state: types.StateRunning, audioRun: 1, last: time.Now().Add(-3 * time.Second)},
		{name: "old run", state: types.StateRunning, audioRun: 2, last: time.Now()},
		{name: "outage awaiting confirmation", state: types.StateRunning, audioRun: 1, last: time.Now(), lost: time.Now().Add(-time.Minute)},
		{name: "stopped", state: types.StateStopped, audioRun: 1, last: time.Now()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := &Encoder{state: tt.state, sourceRunID: 1, audioRunID: tt.audioRun, lastAudioAt: tt.last, inputLostAt: tt.lost}
			if got := e.Status().InputReady; got != tt.want {
				t.Fatalf("InputReady = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestInputReadinessRecoversAfterStalledCapture(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "sleep")
	e.inputStartupTimeout = 300 * time.Millisecond
	e.inputStableDelay = 500 * time.Millisecond
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)
	var attempts atomic.Int32
	e.buildCaptureCommand = func(device, path string) (string, []string, error) {
		mode := "sleep"
		if attempts.Add(1) > 1 {
			mode = "pcm"
		}
		return helperCaptureCommand(mode)(device, path)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, "empty input opens", func() bool { return e.State() == types.StateRunning })
	if e.Status().InputReady {
		t.Fatal("opened input without audio is ready")
	}
	waitForCondition(t, 5*time.Second, "working source confirms recovery", func() bool { return e.Status().InputReady })
	if attempts.Load() < 2 {
		t.Fatal("input became ready before reopening the empty capture")
	}
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	if e.Status().InputReady {
		t.Fatal("stopped input is ready")
	}
}
