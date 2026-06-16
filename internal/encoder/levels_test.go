package encoder

import (
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// TestAudioLevelsNilReadsSilence verifies nil level storage reports silence.
func TestAudioLevelsNilReadsSilence(t *testing.T) {
	e := &Encoder{}
	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() before publish = %+v, want %+v", got, silentAudioLevels)
	}
}

// TestAudioLevelsPublishRoundTrip verifies published levels are read by value.
func TestAudioLevelsPublishRoundTrip(t *testing.T) {
	e := &Encoder{}
	want := audio.AudioLevels{Left: -12.5, Right: -9.25, PeakLeft: -3, PeakRight: -2, ClipLeft: 1}
	e.updateAudioLevels(&want)
	if got := e.AudioLevels(); got != want {
		t.Errorf("AudioLevels() = %+v, want %+v", got, want)
	}

	e.resetAudioLevels()
	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() after reset = %+v, want %+v", got, silentAudioLevels)
	}
}

// TestResetAudioLevelsClearsImbalance verifies the silent snapshot wipes an
// active channel imbalance, so a stopped encoder never reports stale imbalance.
func TestResetAudioLevelsClearsImbalance(t *testing.T) {
	e := &Encoder{}
	e.updateAudioLevels(&audio.AudioLevels{
		ChannelImbalance:      true,
		ChannelImbalanceLevel: audio.ImbalanceLevelActive,
		ImbalanceDB:           40,
		BalanceDB:             40,
	})

	e.resetAudioLevels()

	got := e.AudioLevels()
	if got.ChannelImbalance || got.ChannelImbalanceLevel == audio.ImbalanceLevelActive || got.ImbalanceDB != 0 {
		t.Fatalf("resetAudioLevels did not clear imbalance: %+v", got)
	}
}

// TestRunDistributorPublishesSilenceOnExit verifies the distributor's final
// publish clears the last live level.
func TestRunDistributorPublishesSilenceOnExit(t *testing.T) {
	e := &Encoder{
		state:        types.StateRunning,
		stopChan:     make(chan struct{}),
		sourceStdout: io.NopCloser(strings.NewReader("")), // first Read returns EOF
		sourceRunID:  1,
	}
	e.updateAudioLevels(&audio.AudioLevels{Left: -3, Right: -3, PeakLeft: -1, PeakRight: -1})

	e.runDistributor(1) // returns on EOF; the deferred resetAudioLevels runs

	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() after distributor exit = %+v, want silence %+v", got, silentAudioLevels)
	}
}

// TestAudioLevelsConcurrent keeps AudioLevels race-free under concurrent
// publishers. Run with `go test -race`.
func TestAudioLevelsConcurrent(t *testing.T) {
	e := &Encoder{}
	e.resetAudioLevels()

	const publishers = 8
	const readers = 8
	const iterations = 5000

	var wg sync.WaitGroup

	for p := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iterations {
				if i%64 == 0 {
					e.resetAudioLevels()
					continue
				}
				e.updateAudioLevels(&audio.AudioLevels{
					Left:    float64(-i),
					Right:   float64(p),
					Silence: i%2 == 0,
				})
			}
		}()
	}

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				levels := e.AudioLevels()
				// Touch every word so a torn read cannot be optimized away.
				_ = levels.Left + levels.Right + levels.PeakLeft + levels.PeakRight
			}
		}()
	}

	wg.Wait()
}
