package encoder

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
)

func singleChannelPCM(frames int, left, right uint16) []byte {
	buf := make([]byte, frames*4)
	for i := range frames {
		binary.LittleEndian.PutUint16(buf[i*4:], left)
		binary.LittleEndian.PutUint16(buf[i*4+2:], right)
	}
	return buf
}

func TestDistributorCallbackPreservesChannelOrientation(t *testing.T) {
	t.Parallel()

	const amplitude = 16384
	tests := []struct {
		name                string
		left                uint16
		right               uint16
		wantPositiveBalance bool
	}{
		{name: "left only", left: amplitude, wantPositiveBalance: true},
		{name: "right only", right: amplitude},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
			if err := cfg.Load(); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			orchestrator := notify.NewAlertOrchestrator(cfg, notify.NewDispatcher())
			t.Cleanup(orchestrator.Close)
			var got *audio.AudioLevels
			d := NewDistributor(DistributorConfig{
				SilenceDetect:     audio.NewSilenceDetector(),
				ImbalanceDetect:   audio.NewImbalanceDetector(),
				AlertOrchestrator: orchestrator,
				PeakHolder:        audio.NewPeakHolder(),
				Config:            cfg,
				Callback:          func(l *audio.AudioLevels) { got = l },
			})

			d.ProcessSamples(singleChannelPCM(LevelUpdateSamples, tt.left, tt.right))

			if got == nil {
				t.Fatal("level callback not invoked; metering window not filled")
			}
			if got.ImbalanceDB <= 0 {
				t.Fatalf("ImbalanceDB = %v, want > 0 for a single-channel signal", got.ImbalanceDB)
			}
			if tt.wantPositiveBalance && got.BalanceDB <= 0 {
				t.Fatalf("BalanceDB = %v, want > 0 when left is louder", got.BalanceDB)
			}
			if !tt.wantPositiveBalance && got.BalanceDB >= 0 {
				t.Fatalf("BalanceDB = %v, want < 0 when right is louder", got.BalanceDB)
			}
		})
	}
}
