package encoder

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// AudioLevelCallback is a function that receives audio level updates.
type AudioLevelCallback func(levels *audio.AudioLevels)

// Distributor distributes PCM audio to multiple streams.
type Distributor struct {
	levelData          *audio.LevelData
	silenceDetect      *audio.SilenceDetector
	imbalanceDetect    *audio.ImbalanceDetector
	alertOrchestrator  *notify.AlertOrchestrator
	silenceDumpManager *silencedump.Manager
	peakHolder         *audio.PeakHolder
	config             *config.Config
	callback           AudioLevelCallback
}

// DistributorConfig holds the parameters for creating a Distributor.
type DistributorConfig struct {
	SilenceDetect      *audio.SilenceDetector
	ImbalanceDetect    *audio.ImbalanceDetector
	AlertOrchestrator  *notify.AlertOrchestrator
	SilenceDumpManager *silencedump.Manager
	PeakHolder         *audio.PeakHolder
	Config             *config.Config
	Callback           AudioLevelCallback
}

// NewDistributor returns a new Distributor.
func NewDistributor(cfg DistributorConfig) *Distributor {
	return &Distributor{
		levelData:          &audio.LevelData{},
		silenceDetect:      cfg.SilenceDetect,
		imbalanceDetect:    cfg.ImbalanceDetect,
		alertOrchestrator:  cfg.AlertOrchestrator,
		silenceDumpManager: cfg.SilenceDumpManager,
		peakHolder:         cfg.PeakHolder,
		config:             cfg.Config,
		callback:           cfg.Callback,
	}
}

// ProcessSamples processes a buffer of PCM audio samples.
func (d *Distributor) ProcessSamples(buf []byte) {
	audio.ProcessSamples(buf, d.levelData)

	// Update levels periodically
	if d.levelData.SampleCount >= LevelUpdateSamples {
		levels := audio.CalculateLevels(d.levelData)

		now := time.Now()

		// Update detector settings from config (allows dynamic updates).
		cfg := d.config.DetectorSettings()
		d.peakHolder.SetHoldDuration(time.Duration(cfg.PeakHoldMs) * time.Millisecond)

		heldPeakL, heldPeakR := d.peakHolder.Update(levels.PeakLeft, levels.PeakRight, now)

		// Silence detection (fresh config snapshot for dynamic updates)
		silenceCfg := audio.SilenceConfig{
			Threshold:  cfg.SilenceThreshold,
			DurationMs: cfg.SilenceDurationMs,
			RecoveryMs: cfg.SilenceRecoveryMs,
		}
		silenceEvent := d.silenceDetect.Update(levels.RMSLeft, levels.RMSRight, silenceCfg, now)

		// Use the silence threshold as a presence floor so all-silent audio stays
		// a silence condition, not an imbalance.
		imbalanceCfg := audio.ImbalanceConfig{
			ThresholdDB:     cfg.ChannelImbalanceThreshold,
			DurationMs:      cfg.ChannelImbalanceDurationMs,
			RecoveryMs:      cfg.ChannelImbalanceRecoveryMs,
			PresenceFloorDB: cfg.SilenceThreshold,
		}
		imbalanceEvent := d.imbalanceDetect.Update(levels.RMSLeft, levels.RMSRight, imbalanceCfg, now)

		// Delegate notification handling to the alert orchestrator (separation of concerns)
		d.alertOrchestrator.HandleSilenceEvent(silenceEvent)
		d.alertOrchestrator.HandleChannelImbalanceEvent(&imbalanceEvent)

		// Forward silence events to dump manager for capture
		if d.silenceDumpManager != nil {
			d.silenceDumpManager.HandleSilenceEvent(silenceEvent)
		}

		if d.callback != nil {
			// updateAudioLevels stores this pointer, so each snapshot must be fresh.
			d.callback(&audio.AudioLevels{
				Left:                       levels.RMSLeft,
				Right:                      levels.RMSRight,
				PeakLeft:                   heldPeakL,
				PeakRight:                  heldPeakR,
				Silence:                    silenceEvent.InSilence,
				SilenceDurationMs:          silenceEvent.DurationMs,
				SilenceLevel:               silenceEvent.Level,
				ChannelImbalance:           imbalanceEvent.InImbalance,
				ChannelImbalanceDurationMs: imbalanceEvent.DurationMs,
				ChannelImbalanceLevel:      imbalanceEvent.Level,
				BalanceDB:                  imbalanceEvent.BalanceDB,
				ImbalanceDB:                imbalanceEvent.ImbalanceDB,
				ClipLeft:                   levels.ClipLeft,
				ClipRight:                  levels.ClipRight,
			})
		}

		d.levelData.Reset()
	}
}
