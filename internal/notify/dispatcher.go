package notify

import (
	"context"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

// AlertChannel defines the delivery contract shared by webhook, email, and
// Zabbix notification backends.
type AlertChannel interface {
	// Name returns the stable channel identifier used in logs and event labels.
	Name() string

	// IsConfiguredForSilence reports whether silence events have the backend
	// settings required before per-event subscriptions are checked.
	IsConfiguredForSilence(cfg *config.Snapshot) bool
	// IsConfiguredForImbalance reports whether channel imbalance events have the
	// backend settings required before per-event subscriptions are checked.
	IsConfiguredForImbalance(cfg *config.Snapshot) bool
	// IsConfiguredForUpload reports whether upload-abandonment events have the
	// backend settings required before dispatch.
	IsConfiguredForUpload(cfg *config.Snapshot) bool

	// SubscribesSilenceStart reports whether a configured backend wants
	// silence-start events.
	SubscribesSilenceStart(cfg *config.Snapshot) bool
	// SubscribesSilenceEnd reports whether a configured backend wants
	// silence-recovery events.
	SubscribesSilenceEnd(cfg *config.Snapshot) bool
	// SubscribesChannelImbalanceStart reports whether a configured backend wants
	// channel-imbalance start events.
	SubscribesChannelImbalanceStart(cfg *config.Snapshot) bool
	// SubscribesChannelImbalanceEnd reports whether a configured backend wants
	// channel-imbalance recovery events.
	SubscribesChannelImbalanceEnd(cfg *config.Snapshot) bool
	// SubscribesAudioDump reports whether a configured backend wants audio-dump
	// notifications after silence recovery.
	SubscribesAudioDump(cfg *config.Snapshot) bool

	// SendSilenceStart delivers a silence-start alert with the current stereo levels.
	SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error
	// SendSilenceEnd delivers a silence-recovery alert with duration and final levels.
	SendSilenceEnd(ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error
	// SendChannelImbalanceStart delivers a confirmed channel-imbalance alert.
	SendChannelImbalanceStart(ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData) error
	// SendChannelImbalanceEnd delivers a channel-balance recovery alert.
	SendChannelImbalanceEnd(ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData) error
	// SendAudioDump delivers an audio dump notification, or returns an error if
	// the backend cannot carry attachments.
	SendAudioDump(
		ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
		result *silencedump.EncodeResult,
	) error
	// SendUploadAbandoned delivers an alert for a recording that exhausted upload retries.
	SendUploadAbandoned(ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData) error
}

// UploadAbandonedData contains the retry context sent when a recorder upload is abandoned.
type UploadAbandonedData struct {
	RecorderName string
	Filename     string
	S3Key        string
	LastError    string
	RetryCount   int
}

// ChannelImbalanceData contains the measured levels and thresholds for an
// imbalance lifecycle event.
type ChannelImbalanceData struct {
	LevelL      float64
	LevelR      float64
	BalanceDB   float64
	ImbalanceDB float64
	ThresholdDB float64
	DurationMs  int64
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

// dispatch fans one event kind out to every channel in list that passes the
// subscribes predicate, delivering via send on a per-channel goroutine. Each
// goroutine receives its own Snapshot copy so channels can never observe each
// other's mutations.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func dispatch(
	list []AlertChannel, cfg config.Snapshot, kind string,
	subscribes func(AlertChannel, *config.Snapshot) bool,
	send func(ch AlertChannel, cfg *config.Snapshot) error,
) {
	for _, ch := range list {
		if !subscribes(ch, &cfg) {
			continue
		}
		go func(ch AlertChannel, cfg config.Snapshot) {
			logNotifyResult(func() error { return send(ch, &cfg) }, ch.Name(), kind)
		}(ch, cfg)
	}
}

// DispatchSilenceStart sends silence-start notifications to subscribed channels from the active set.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchSilenceStart(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, levelL, levelR float64,
) {
	dispatch(active, cfg, "silence_start", AlertChannel.SubscribesSilenceStart,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendSilenceStart(ctx, cfg, levelL, levelR)
		})
}

// DispatchSilenceEnd sends silence-end notifications to the active channel subset.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchSilenceEnd(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, durationMS int64, levelL, levelR float64,
) {
	dispatch(active, cfg, "silence_end", AlertChannel.SubscribesSilenceEnd,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendSilenceEnd(ctx, cfg, durationMS, levelL, levelR)
		})
}

// DispatchChannelImbalanceStart sends imbalance-start notifications to subscribed channels.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchChannelImbalanceStart(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, data ChannelImbalanceData,
) {
	dispatch(active, cfg, "channel_imbalance_start", AlertChannel.SubscribesChannelImbalanceStart,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendChannelImbalanceStart(ctx, cfg, data)
		})
}

// DispatchChannelImbalanceEnd sends imbalance-end notifications to the active channel subset.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchChannelImbalanceEnd(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, data ChannelImbalanceData,
) {
	dispatch(active, cfg, "channel_imbalance_end", AlertChannel.SubscribesChannelImbalanceEnd,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendChannelImbalanceEnd(ctx, cfg, data)
		})
}

// DispatchAudioDump sends audio-dump notifications to the active channel subset.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchAudioDump(
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, durationMS int64,
	levelL, levelR float64, result *silencedump.EncodeResult,
) {
	dispatch(active, cfg, "audio_dump_ready", AlertChannel.SubscribesAudioDump,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendAudioDump(ctx, cfg, durationMS, levelL, levelR, result)
		})
}

// DispatchUploadAbandoned sends upload-abandonment notifications to all configured channels.
//
//nolint:gocritic // hugeParam: intentional; Snapshot is a value type and each goroutine receives its own copy
func (d *Dispatcher) DispatchUploadAbandoned(ctx context.Context, cfg config.Snapshot, params UploadAbandonedData) {
	dispatch(d.channels, cfg, "upload_abandoned", AlertChannel.IsConfiguredForUpload,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			return ch.SendUploadAbandoned(ctx, cfg, params)
		})
}
