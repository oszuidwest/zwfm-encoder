//go:build audit

package notify

import (
	"context"
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
)

// silenceSequenceChannel records silence deliveries; SendSilenceStart blocks until released.
type silenceSequenceChannel struct {
	noopAlertChannel
	calls   chan string
	release chan struct{}
	mu      sync.Mutex
	got     []string
}

func (c *silenceSequenceChannel) IsConfiguredForSilence(_ *config.Snapshot) bool { return true }
func (c *silenceSequenceChannel) SubscribesSilenceStart(_ *config.Snapshot) bool { return true }
func (c *silenceSequenceChannel) SubscribesSilenceEnd(_ *config.Snapshot) bool   { return true }
func (c *silenceSequenceChannel) note(kind string) {
	c.mu.Lock()
	c.got = append(c.got, kind)
	c.mu.Unlock()
	c.calls <- kind
}
func (c *silenceSequenceChannel) SendSilenceStart(_ context.Context, _ *config.Snapshot, _, _ float64) error {
	c.note("start")
	<-c.release
	return nil
}
func (c *silenceSequenceChannel) SendSilenceEnd(_ context.Context, _ *config.Snapshot, _ int64, _, _ float64) error {
	c.note("end")
	return nil
}

// Input alerts are delivered in order per channel (TestInputRestoredNeverOvertakesSlowInputLost);
// silence alerts take a separate, unordered dispatch path.
func TestAuditSilenceEndNeverOvertakesSlowSilenceStart(t *testing.T) {
	ch := &silenceSequenceChannel{calls: make(chan string, 4), release: make(chan struct{})}
	o := NewAlertOrchestrator(config.New(""), NewDispatcher(ch))
	t.Cleanup(o.Close)
	t.Cleanup(func() {
		select {
		case <-ch.release:
		default:
			close(ch.release)
		}
	})

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true, IncidentID: 1})
	if got := awaitCall(t, ch.calls, "SendSilenceStart"); got != "start" {
		t.Fatalf("first delivery = %q, want start", got)
	}
	o.HandleSilenceEvent(audio.SilenceEvent{JustRecovered: true, IncidentID: 1, TotalDurationMs: 1000})
	assertNoCall(t, ch.calls, "SendSilenceEnd while SendSilenceStart is still in flight")
}

// A settings save resets the detectors through ApplySettingsEffects. An incident that
// was open at that moment must be closed before a new one is opened.
func TestAuditResetDuringSilenceKeepsStartEndAlternating(t *testing.T) {
	ch := &silenceSequenceChannel{calls: make(chan string, 4), release: make(chan struct{})}
	close(ch.release)
	o := NewAlertOrchestrator(config.New(""), NewDispatcher(ch))
	t.Cleanup(o.Close)

	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true, IncidentID: 1})
	awaitCall(t, ch.calls, "first SendSilenceStart")
	o.Reset() // the detector restarts and re-confirms the ongoing silence
	o.HandleSilenceEvent(audio.SilenceEvent{JustEntered: true, IncidentID: 1})
	second := awaitCall(t, ch.calls, "delivery after reset")
	if second != "end" {
		t.Fatalf("delivery after reset = %q, want end: the incident open before Reset never received its recovery", second)
	}
}
