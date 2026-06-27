package encoder

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func benchmarkBalancedPCM(frames int) []byte {
	buf := make([]byte, frames*4)
	for i := range frames {
		binary.LittleEndian.PutUint16(buf[i*4:], 16000)
		binary.LittleEndian.PutUint16(buf[i*4+2:], 16000)
	}
	return buf
}

func benchmarkConfig(streams, recorders int) *config.Config {
	cfg := config.New("")
	cfg.SilenceDetection = config.SilenceDetectionConfig{
		ThresholdDB: config.DefaultSilenceThreshold,
		DurationMs:  config.DefaultSilenceDurationMs,
		RecoveryMs:  config.DefaultSilenceRecoveryMs,
		PeakHoldMs:  config.DefaultPeakHoldMs,
	}
	cfg.ChannelImbalanceDetection = config.ChannelImbalanceDetectionConfig{
		ThresholdDB: config.DefaultChannelImbalanceThreshold,
		DurationMs:  config.DefaultChannelImbalanceDurationMs,
		RecoveryMs:  config.DefaultChannelImbalanceRecoveryMs,
	}
	cfg.Streaming.Streams = make([]types.Stream, streams)
	for i := range cfg.Streaming.Streams {
		cfg.Streaming.Streams[i] = types.Stream{
			ID:      fmt.Sprintf("stream-%02d", i),
			Enabled: true,
			Mode:    types.StreamModeCaller,
			Host:    "127.0.0.1",
			Port:    9000 + i,
			Codec:   types.CodecPCM,
		}
	}
	cfg.Recording.Recorders = make([]types.Recorder, recorders)
	for i := range cfg.Recording.Recorders {
		cfg.Recording.Recorders[i] = types.Recorder{
			ID:            fmt.Sprintf("recorder-%02d", i),
			Name:          fmt.Sprintf("Recorder %02d", i),
			Enabled:       true,
			Codec:         types.CodecPCM,
			RecordingMode: types.RecordingOnDemand,
			StorageMode:   types.StorageLocal,
			LocalPath:     "/tmp",
		}
	}
	return cfg
}

func BenchmarkDistributorProcessSamples(b *testing.B) {
	pcm := benchmarkBalancedPCM(LevelUpdateSamples)
	for _, size := range []int{0, 1, 4, 16} {
		b.Run(fmt.Sprintf("streams=%d/recorders=%d", size, size), func(b *testing.B) {
			cfg := benchmarkConfig(size, size)
			orchestrator := notify.NewAlertOrchestrator(cfg, notify.NewDispatcher())
			b.Cleanup(orchestrator.Close)

			distributor := NewDistributor(DistributorConfig{
				SilenceDetect:     audio.NewSilenceDetector(),
				ImbalanceDetect:   audio.NewImbalanceDetector(),
				AlertOrchestrator: orchestrator,
				PeakHolder:        audio.NewPeakHolder(),
				Config:            cfg,
			})

			b.ReportAllocs()
			for b.Loop() {
				distributor.ProcessSamples(pcm)
			}
		})
	}
}
