package encoder

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// AudioLevelCallback is invoked with updated audio metrics.
type AudioLevelCallback func(metrics *types.AudioMetrics)

// Distributor handles audio sample processing, level metering, and silence detection.
type Distributor struct {
	levelData       *audio.LevelData
	silenceDetect   *audio.SilenceDetector
	silenceNotifier *notify.SilenceNotifier
	peakHolder      *audio.PeakHolder
	silenceCfg      audio.SilenceConfig
	callback        AudioLevelCallback
}

// NewDistributor creates a new audio distributor with the given configuration and callback.
func NewDistributor(silenceDetect *audio.SilenceDetector, silenceNotifier *notify.SilenceNotifier, peakHolder *audio.PeakHolder, silenceCfg audio.SilenceConfig, callback AudioLevelCallback) *Distributor {
	return &Distributor{
		levelData:       &audio.LevelData{},
		silenceDetect:   silenceDetect,
		silenceNotifier: silenceNotifier,
		peakHolder:      peakHolder,
		silenceCfg:      silenceCfg,
		callback:        callback,
	}
}

// ProcessSamples processes a buffer of audio samples for level metering and silence detection.
func (d *Distributor) ProcessSamples(buf []byte, n int) {
	audio.ProcessSamples(buf, n, d.levelData)

	// Update levels periodically
	if d.levelData.SampleCount >= LevelUpdateSamples {
		levels := audio.CalculateLevels(d.levelData)

		now := time.Now()
		heldPeakL, heldPeakR := d.peakHolder.Update(levels.PeakLeft, levels.PeakRight, now)

		// Silence detection (using snapshot from startup)
		silenceEvent := d.silenceDetect.Update(levels.RMSLeft, levels.RMSRight, d.silenceCfg, now)

		// Delegate notification handling to the notifier (separation of concerns)
		d.silenceNotifier.HandleEvent(silenceEvent)

		if d.callback != nil {
			d.callback(&types.AudioMetrics{
				RMSLeft:         levels.RMSLeft,
				RMSRight:        levels.RMSRight,
				PeakLeft:        heldPeakL,
				PeakRight:       heldPeakR,
				Silence:         silenceEvent.InSilence,
				SilenceDuration: silenceEvent.Duration,
				SilenceLevel:    silenceEvent.Level,
				ClipLeft:        levels.ClipLeft,
				ClipRight:       levels.ClipRight,
			})
		}

		d.levelData.Reset()
	}
}
