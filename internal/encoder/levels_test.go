package encoder

import (
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"io"
	"strings"
	"testing"
)

func TestAudioLevelsNilReadsSilence(t *testing.T) {
	e := &Encoder{}
	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() before publish = %+v, want %+v", got, silentAudioLevels)
	}
}

func TestRunDistributorPublishesSilenceOnExit(t *testing.T) {
	e := &Encoder{
		state:        types.StateRunning,
		stopChan:     make(chan struct{}),
		sourceStdout: io.NopCloser(strings.NewReader("")), // First Read returns EOF.
		sourceRunID:  1,
	}
	e.updateAudioLevels(&audio.AudioLevels{Left: -3, Right: -3, PeakLeft: -1, PeakRight: -1})
	e.runDistributor(1) // Returns on EOF; the deferred resetAudioLevels runs.
	if got := e.AudioLevels(); got != silentAudioLevels {
		t.Errorf("AudioLevels() after distributor exit = %+v, want silence %+v", got, silentAudioLevels)
	}
}
