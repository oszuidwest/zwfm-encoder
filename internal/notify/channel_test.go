package notify

import (
	"context"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestUnconfiguredChannelsSendNothing(t *testing.T) {
	t.Parallel()

	ctx, cfg := context.Background(), &config.Snapshot{}
	for _, channel := range []AlertChannel{&ZabbixChannel{}, &WebhookChannel{}} {
		for name, err := range map[string]error{
			"silence start":           channel.SendSilenceStart(ctx, cfg, -48, -52),
			"silence end":             channel.SendSilenceEnd(ctx, cfg, 1000, -12, -14),
			"channel imbalance start": channel.SendChannelImbalanceStart(ctx, cfg, ChannelImbalanceData{}),
			"channel imbalance end":   channel.SendChannelImbalanceEnd(ctx, cfg, ChannelImbalanceData{}),
			"uploads overdue":         channel.SendUploadsOverdue(ctx, cfg, &UploadsOverdueData{Count: 1}),
			"recording storage":       channel.SendRecordingStorage(ctx, cfg, nil),
		} {
			if err != nil {
				t.Errorf("unconfigured %s %s returned error: %v", channel.Name(), name, err)
			}
		}
	}
}

// assertSubscribesOnlyToEnabledEvents enables one event at a time and checks
// that exactly the matching Subscribes* method reports true, and only when the
// channel itself is configured.
func assertSubscribesOnlyToEnabledEvents(
	t *testing.T,
	channel AlertChannel,
	supportsAudioDump bool,
	snapshot func(events types.EventSubscriptions, configured bool) *config.Snapshot,
) {
	t.Helper()

	events := []struct {
		name       string
		enabled    types.EventSubscriptions
		subscribes func(*config.Snapshot) bool
	}{
		{"silence start", types.EventSubscriptions{SilenceStart: true}, channel.SubscribesSilenceStart},
		{"silence end", types.EventSubscriptions{SilenceEnd: true}, channel.SubscribesSilenceEnd},
		{"channel imbalance start", types.EventSubscriptions{ChannelImbalanceStart: true}, channel.SubscribesChannelImbalanceStart},
		{"channel imbalance end", types.EventSubscriptions{ChannelImbalanceEnd: true}, channel.SubscribesChannelImbalanceEnd},
		{"audio dump", types.EventSubscriptions{AudioDump: true}, channel.SubscribesAudioDump},
	}
	for _, enabled := range events {
		for _, event := range events {
			want := event.name == enabled.name && (event.name != "audio dump" || supportsAudioDump)
			if got := event.subscribes(snapshot(enabled.enabled, true)); got != want {
				t.Errorf("with only %q enabled, %s subscription = %t, want %t", enabled.name, event.name, got, want)
			}
		}
		if enabled.subscribes(snapshot(enabled.enabled, false)) {
			t.Errorf("%s subscription = true for an unconfigured channel", enabled.name)
		}
	}
}
