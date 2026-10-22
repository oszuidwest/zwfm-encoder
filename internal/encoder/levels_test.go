package encoder

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
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

// The distributor passes whole frames only, so a read that ends inside a frame
// cannot swap the channels for its consumers.
func TestRunDistributorKeepsFramesWhole(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	e.state, e.sourceRunID, e.stopChan = types.StateRunning, 1, make(chan struct{})
	pcm := bytes.Repeat([]byte{0xff, 0x7f, 0, 0}, LevelUpdateSamples) // Left loud, right silent.
	reads := [][]byte{pcm[:2], pcm[2:]}
	var levels audio.AudioLevels
	e.sourceStdout = inputReadFunc(func(p []byte) (int, error) {
		if len(reads) == 0 {
			levels = e.AudioLevels()
			return 0, io.EOF
		}
		n := copy(p, reads[0])
		if reads[0] = reads[0][n:]; len(reads[0]) == 0 {
			reads = reads[1:]
		}
		return n, nil
	})
	e.runDistributor(1)
	if levels.Left <= levels.Right {
		t.Fatalf("levels = %+v, want the left channel loud", levels)
	}
}
