package encoder

import (
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
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
