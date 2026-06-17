package encoder

import (
	"encoding/binary"
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"path/filepath"
	"testing"
)

func deadRightChannelPCM(frames int) []byte {
	buf := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(buf[i*4:], 16000) // Left channel is approximately -6 dBFS.
		binary.LittleEndian.PutUint16(buf[i*4+2:], 0)   // Right channel is silent.
	}
	return buf
}

func TestDistributorCallbackCarriesImbalanceFields(t *testing.T) {
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
	d.ProcessSamples(deadRightChannelPCM(LevelUpdateSamples))
	if got == nil {
		t.Fatal("level callback not invoked; metering window not filled")
	}
	if got.ImbalanceDB <= 0 {
		t.Fatalf("ImbalanceDB = %v, want > 0 for a dead right channel", got.ImbalanceDB)
	}
	if got.BalanceDB <= 0 {
		t.Fatalf("BalanceDB = %v, want > 0 (left louder)", got.BalanceDB)
	}
}
