package encoder

import (
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// TestAudioLevelsNilReadsSilence verifies that AudioLevels() returns the silent
// snapshot before any levels have been published (the atomic.Pointer is nil).
func TestAudioLevelsNilReadsSilence(t *testing.T) {
	e := &Encoder{}
	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() before publish = %+v, want %+v", got, silentAudioLevels)
	}
}

// TestAudioLevelsPublishRoundTrip verifies that a published snapshot is read back
// by value.
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

// TestAudioLevelsConcurrent exercises AudioLevels() against concurrent
// updateAudioLevels()/resetAudioLevels() publishers. Storing the levels in an
// atomic.Pointer makes this race-free; the previous implementation returned a
// cached copy without synchronization on the TryRLock fallback path, which the
// race detector flags. Run with `go test -race`.
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
