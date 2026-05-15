package notify

import (
	"context"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// AlertChannel is a notification delivery channel (webhook, email, Zabbix).
type AlertChannel interface {
	Name() string
	IsConfiguredForSilence(cfg *config.Snapshot) bool
	IsConfiguredForUpload(cfg *config.Snapshot) bool
	SubscribesSilenceStart(cfg *config.Snapshot) bool
	SubscribesSilenceEnd(cfg *config.Snapshot) bool
	SubscribesAudioDump(cfg *config.Snapshot) bool
	SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error
	SendSilenceEnd(ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error
	SendAudioDump(
		ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
		result *silencedump.EncodeResult,
	) error
	SendUploadAbandoned(ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData) error
}

// UploadAbandonedData contains details about an abandoned upload for notification dispatch.
type UploadAbandonedData struct {
	RecorderName string
	Filename     string
	S3Key        string
	LastError    string
	RetryCount   int
}

// silenceEventData groups audio level and silence parameters shared across
// silence-start, silence-end, and audio-dump notification functions.
type silenceEventData struct {
	LevelL     float64                   // dB
	LevelR     float64                   // dB
	Threshold  float64                   // dB
	DurationMs int64                     // ms; zero for silence-start events
	Dump       *silencedump.EncodeResult // nil except for audio-dump events
}

// Dispatcher routes alert events to alert channels.
type Dispatcher struct {
	channels []AlertChannel
}

// NewDispatcher creates a dispatcher with the given channels.
func NewDispatcher(channels ...AlertChannel) *Dispatcher {
	return &Dispatcher{channels: channels}
}

// Channels returns the full channel list.
func (d *Dispatcher) Channels() []AlertChannel {
	return d.channels
}

// DispatchSilenceStart sends silence-start notifications to subscribed channels from the active set.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchSilenceStart(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, levelL, levelR float64,
) {
	for _, ch := range active {
		if !ch.SubscribesSilenceStart(&cfg) {
			continue
		}
		go func(ch AlertChannel, cfg config.Snapshot) {
			logNotifyResult(
				func() error { return ch.SendSilenceStart(ctx, &cfg, levelL, levelR) },
				ch.Name(),
				"silence_start",
			)
		}(ch, cfg)
	}
}

// DispatchSilenceEnd sends silence-end notifications to the active channel subset.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchSilenceEnd(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, durationMS int64, levelL, levelR float64,
) {
	for _, ch := range active {
		if !ch.SubscribesSilenceEnd(&cfg) {
			continue
		}
		go func(ch AlertChannel, cfg config.Snapshot) {
			logNotifyResult(
				func() error { return ch.SendSilenceEnd(ctx, &cfg, durationMS, levelL, levelR) },
				ch.Name(),
				"silence_end",
			)
		}(ch, cfg)
	}
}

// DispatchAudioDump sends audio-dump notifications to the active channel subset.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchAudioDump(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, durationMS int64,
	levelL, levelR float64, result *silencedump.EncodeResult,
) {
	for _, ch := range active {
		if !ch.SubscribesAudioDump(&cfg) {
			continue
		}
		go func(ch AlertChannel, cfg config.Snapshot) {
			logNotifyResult(
				func() error { return ch.SendAudioDump(ctx, &cfg, durationMS, levelL, levelR, result) },
				ch.Name(),
				"audio_dump_ready",
			)
		}(ch, cfg)
	}
}

// DispatchUploadAbandoned sends upload-abandonment notifications to all configured channels.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchUploadAbandoned(ctx context.Context, cfg config.Snapshot, params UploadAbandonedData) {
	for _, ch := range d.channels {
		if !ch.IsConfiguredForUpload(&cfg) {
			continue
		}
		go func(ch AlertChannel, cfg config.Snapshot) {
			logNotifyResult(
				func() error { return ch.SendUploadAbandoned(ctx, &cfg, params) },
				ch.Name(),
				"upload_abandoned",
			)
		}(ch, cfg)
	}
}
