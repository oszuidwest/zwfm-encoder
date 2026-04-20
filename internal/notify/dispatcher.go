package notify

import (
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
	SendSilenceStart(cfg *config.Snapshot, levelL, levelR float64) error
	SendSilenceEnd(cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error
	SendAudioDump(cfg *config.Snapshot, durationMS int64, levelL, levelR float64, result *silencedump.EncodeResult) error
	SendUploadAbandoned(cfg *config.Snapshot, params UploadAbandonedData) error
}

// UploadAbandonedData contains details about an abandoned upload for notification dispatch.
type UploadAbandonedData struct {
	RecorderName string
	Filename     string
	S3Key        string
	LastError    string
	RetryCount   int
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
func (d *Dispatcher) DispatchSilenceStart(active []AlertChannel, cfg config.Snapshot, levelL, levelR float64) {
	for _, ch := range active {
		if !ch.SubscribesSilenceStart(&cfg) {
			continue
		}
		ch := ch
		cfg := cfg // per-goroutine copy; prevents a future mutating Send* from racing with siblings
		go func() {
			logNotifyResult(
				func() error { return ch.SendSilenceStart(&cfg, levelL, levelR) },
				ch.Name(),
				"silence_start",
			)
		}()
	}
}

// DispatchSilenceEnd sends silence-end notifications to the active channel subset.
func (d *Dispatcher) DispatchSilenceEnd(active []AlertChannel, cfg config.Snapshot, durationMS int64, levelL, levelR float64) {
	for _, ch := range active {
		if !ch.SubscribesSilenceEnd(&cfg) {
			continue
		}
		ch := ch
		cfg := cfg
		go func() {
			logNotifyResult(
				func() error { return ch.SendSilenceEnd(&cfg, durationMS, levelL, levelR) },
				ch.Name(),
				"silence_end",
			)
		}()
	}
}

// DispatchAudioDump sends audio-dump notifications to the active channel subset.
func (d *Dispatcher) DispatchAudioDump(active []AlertChannel, cfg config.Snapshot, durationMS int64, levelL, levelR float64, result *silencedump.EncodeResult) {
	for _, ch := range active {
		if !ch.SubscribesAudioDump(&cfg) {
			continue
		}
		ch := ch
		cfg := cfg
		go func() {
			logNotifyResult(
				func() error { return ch.SendAudioDump(&cfg, durationMS, levelL, levelR, result) },
				ch.Name(),
				"audio_dump_ready",
			)
		}()
	}
}

// DispatchUploadAbandoned sends upload-abandonment notifications to all configured channels.
func (d *Dispatcher) DispatchUploadAbandoned(cfg config.Snapshot, params UploadAbandonedData) {
	for _, ch := range d.channels {
		if !ch.IsConfiguredForUpload(&cfg) {
			continue
		}
		ch := ch
		cfg := cfg
		go func() {
			logNotifyResult(
				func() error { return ch.SendUploadAbandoned(&cfg, params) },
				ch.Name(),
				"upload_abandoned",
			)
		}()
	}
}
