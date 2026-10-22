package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
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
	// IsConfiguredForUpload reports whether upload and recording-storage events
	// have the backend settings required before dispatch.
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
	// notifications after an audio incident recovers.
	SubscribesAudioDump(cfg *config.Snapshot) bool

	// SendSilenceStart delivers a silence-start alert with the current stereo levels.
	SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error
	// SendSilenceEnd delivers a silence-recovery alert with duration and final levels.
	SendSilenceEnd(ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error
	// SendChannelImbalanceStart delivers a confirmed channel-imbalance alert.
	SendChannelImbalanceStart(ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData) error
	// SendChannelImbalanceEnd delivers a channel-balance recovery alert.
	SendChannelImbalanceEnd(ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData) error
	// SendAudioDump delivers a dump or errors if attachments are unsupported.
	// Implementations must treat data as read-only.
	SendAudioDump(ctx context.Context, cfg *config.Snapshot, data *AudioDumpData) error
	// SendUploadsOverdue delivers the overdue state of recording uploads: an
	// alert when recordings wait too long for S3, or their recovery.
	SendUploadsOverdue(ctx context.Context, cfg *config.Snapshot, data *UploadsOverdueData) error
	// SendRecordingStorage delivers the current aggregate recording-storage state.
	SendRecordingStorage(ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus) error
	// SendInputLost delivers an alert when audio capture from the input stops.
	SendInputLost(ctx context.Context, cfg *config.Snapshot, data InputData) error
	// SendInputRestored delivers an alert when audio capture runs stably again.
	SendInputRestored(ctx context.Context, cfg *config.Snapshot, data InputData) error
}

// InputData describes an audio input loss or restoration.
type InputData struct {
	Input      string
	Error      string // set for input loss
	DurationMs int64  // set for input restoration
}

// UploadsOverdueData describes recordings that wait more than two hours for
// S3, or with Overdue false that none do any longer.
type UploadsOverdueData struct {
	Overdue   bool
	Count     int
	Oldest    time.Time
	Recorders []string
	LastError string
}

func (d *UploadsOverdueData) message() (event, message string) {
	if !d.Overdue {
		return "upload_overdue_cleared", "All overdue recordings have been uploaded."
	}
	message = fmt.Sprintf("%d recordings have waited more than 2 hours for S3 (oldest since %s; recorders: %s).",
		d.Count, d.Oldest.Format(time.RFC3339), strings.Join(d.Recorders, ", "))
	if d.LastError != "" {
		message += " Last error: " + d.LastError
	}
	return "upload_overdue", message
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

// AudioDumpData contains the incident context sent with a completed audio dump.
type AudioDumpData struct {
	Trigger     silencedump.Trigger
	IncidentID  audio.IncidentID
	LevelL      float64
	LevelR      float64
	BalanceDB   *float64 // set for channel-imbalance dumps
	ImbalanceDB *float64 // set for channel-imbalance dumps
	ThresholdDB float64
	DurationMs  int64
	Result      *silencedump.EncodeResult
}

// silenceEventData groups audio level and silence parameters shared by the
// silence-start and silence-end notification functions.
type silenceEventData struct {
	LevelL     float64 // dB
	LevelR     float64 // dB
	Threshold  float64 // dB
	DurationMs int64   // ms; zero for silence-start events
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

// dispatch sends to subscribed channels in separate goroutines. Each receives a
// shallow Snapshot copy; channels must not mutate its shared slice contents.
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
	ctx context.Context, active []AlertChannel, cfg config.Snapshot, data AudioDumpData,
) {
	dispatch(active, cfg, "audio_dump_ready", AlertChannel.SubscribesAudioDump,
		func(ch AlertChannel, cfg *config.Snapshot) error {
			data := data // Give each channel its own struct value.
			return ch.SendAudioDump(ctx, cfg, &data)
		})
}
