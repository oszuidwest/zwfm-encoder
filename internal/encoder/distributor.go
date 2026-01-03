package encoder

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// AudioLevelCallback receives audio level updates from the distributor.
type AudioLevelCallback func(metrics *types.AudioMetrics)

// Distributor fans out PCM audio data to outputs and calculates audio levels.
type Distributor struct {
	levelData       *audio.LevelData
	silenceDetect   *audio.SilenceDetector
	silenceNotifier *notify.SilenceNotifier
	peakHolder      *audio.PeakHolder
	config          *config.Config
	callback        AudioLevelCallback
}

// NewDistributor creates a new audio distributor with the given configuration and callback.
func NewDistributor(silenceDetect *audio.SilenceDetector, silenceNotifier *notify.SilenceNotifier, peakHolder *audio.PeakHolder, cfg *config.Config, callback AudioLevelCallback) *Distributor {
	return &Distributor{
		levelData:       &audio.LevelData{},
		silenceDetect:   silenceDetect,
		silenceNotifier: silenceNotifier,
		peakHolder:      peakHolder,
		config:          cfg,
		callback:        callback,
	}
}

// ProcessSamples analyzes audio samples and updates audio level metrics.
func (d *Distributor) ProcessSamples(buf []byte, n int) {
	audio.ProcessSamples(buf, n, d.levelData)

	// Update levels periodically
	if d.levelData.SampleCount >= LevelUpdateSamples {
		levels := audio.CalculateLevels(d.levelData)

		now := time.Now()
		heldPeakL, heldPeakR := d.peakHolder.Update(levels.PeakLeft, levels.PeakRight, now)

		// Silence detection (fresh config snapshot for dynamic updates)
		cfg := d.config.Snapshot()
		silenceCfg := audio.SilenceConfig{
			Threshold:  cfg.SilenceThreshold,
			DurationMs: cfg.SilenceDurationMs,
			RecoveryMs: cfg.SilenceRecoveryMs,
		}
		silenceEvent := d.silenceDetect.Update(levels.RMSLeft, levels.RMSRight, silenceCfg, now)

		// Delegate notification handling to the notifier (separation of concerns)
		d.silenceNotifier.HandleEvent(silenceEvent)

		if d.callback != nil {
			d.callback(&types.AudioMetrics{
				RMSLeft:           levels.RMSLeft,
				RMSRight:          levels.RMSRight,
				PeakLeft:          heldPeakL,
				PeakRight:         heldPeakR,
				Silence:           silenceEvent.InSilence,
				SilenceDurationMs: silenceEvent.DurationMs,
				SilenceLevel:      silenceEvent.Level,
				ClipLeft:          levels.ClipLeft,
				ClipRight:         levels.ClipRight,
			})
		}

		d.levelData.Reset()
	}
}
