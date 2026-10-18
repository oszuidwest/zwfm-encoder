package encoder

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

type inputReadFunc func([]byte) (int, error)

func (f inputReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestInputReadinessClearsAfterFlushedPCMEnds(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	e.state, e.sourceRunID = types.StateRunning, 1
	e.stopChan = make(chan struct{})
	t.Cleanup(func() { e.state = types.StateStopped })
	pcm := bytes.NewReader(singleChannelPCM(LevelUpdateSamples, 16384, 16384))
	sawTail := false
	e.sourceStdout = io.NopCloser(inputReadFunc(func(p []byte) (int, error) {
		n, err := pcm.Read(p)
		if err == io.EOF {
			// The media timeout flush reaches consumers before teardown completes.
			sawTail = e.Status().InputReady && e.AudioLevels().Left > -10
		}
		return n, err
	}))
	e.runDistributor(1)
	if !sawTail {
		t.Fatal("flushed PCM did not reach readiness and the level meter")
	}
	if e.Status().InputReady {
		t.Fatal("input is ready after the terminal read while source teardown is pending")
	}
	// Supervision reads these to report the source's own exit error, not a stall.
	if _, last := e.audioTimes(1); last.IsZero() {
		t.Fatal("distributor exit erased the run's audio history")
	}
}

func TestRestartedRunIsReadyAfterEarlierRunEnded(t *testing.T) {
	t.Parallel()
	e := &Encoder{state: types.StateRunning, sourceRunID: 1}
	e.noteAudio(1)
	e.runDistributor(1) // run 1's PCM ends
	e.sourceRunID = 2
	e.noteAudio(2)
	e.runDistributor(1) // a stale exit of the old run
	if !e.Status().InputReady {
		t.Fatal("the restarted run is not ready after an earlier run's PCM ended")
	}
}

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
