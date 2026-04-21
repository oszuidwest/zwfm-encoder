package types

import "testing"

func TestEventSubscriptionsToZabbixEventSubscriptionsOmitsAudioDump(t *testing.T) {
	t.Parallel()

	got := (EventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   false,
		AudioDump:    true,
	}).ToZabbixEventSubscriptions()

	want := ZabbixEventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   false,
	}
	if got != want {
		t.Fatalf("ToZabbixEventSubscriptions() = %+v, want %+v", got, want)
	}
}

func TestZabbixEventSubscriptionsRoundTrip(t *testing.T) {
	t.Parallel()

	start := ZabbixEventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   true,
	}

	got := start.ToEventSubscriptions().ToZabbixEventSubscriptions()
	if got != start {
		t.Fatalf("round-trip = %+v, want %+v", got, start)
	}
}
