package notify

import (
	"strings"
	"testing"
)

func TestBuildChannelImbalanceEmails(t *testing.T) {
	t.Parallel()
	startSubject, startBody := buildChannelImbalanceStartEmail(
		"ZuidWest FM",
		"27 Jun 2026 12:00:00 CEST",
		ChannelImbalanceData{
			LevelL:      -6,
			LevelR:      -30,
			BalanceDB:   24,
			ImbalanceDB: 24,
			ThresholdDB: 12,
		},
	)
	if startSubject != "[ALERT] Channel Imbalance Detected - ZuidWest FM" {
		t.Fatalf("start subject = %q", startSubject)
	}
	for _, want := range []string{
		"27 Jun 2026 12:00:00 CEST",
		"Level: Left -6.0 dB / Right -30.0 dB",
		"Balance: 24.0 dB (left louder)",
		"Imbalance: 24.0 dB",
		"Threshold: 12.0 dB",
	} {
		if !strings.Contains(startBody, want) {
			t.Fatalf("start body missing %q:\n%s", want, startBody)
		}
	}

	endSubject, endBody := buildChannelImbalanceEndEmail(
		"ZuidWest FM",
		"27 Jun 2026 12:01:00 CEST",
		ChannelImbalanceData{
			LevelL:      -8,
			LevelR:      -8,
			BalanceDB:   0,
			ImbalanceDB: 0,
			ThresholdDB: 12,
			DurationMs:  16000,
		},
	)
	if endSubject != "[OK] Channels Balanced - ZuidWest FM" {
		t.Fatalf("end subject = %q", endSubject)
	}
	for _, want := range []string{
		"27 Jun 2026 12:01:00 CEST",
		"The imbalance lasted 16s.",
		"Final level: Left -8.0 dB / Right -8.0 dB",
		"Final balance: 0.0 dB (balanced)",
		"Final imbalance: 0.0 dB",
		"Threshold: 12.0 dB",
	} {
		if !strings.Contains(endBody, want) {
			t.Fatalf("end body missing %q:\n%s", want, endBody)
		}
	}
}

func TestChannelImbalanceDirection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{name: "left louder", in: 1, want: "left louder"},
		{name: "right louder", in: -1, want: "right louder"},
		{name: "balanced", in: 0, want: "balanced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := channelImbalanceDirection(tt.in); got != tt.want {
				t.Fatalf("channelImbalanceDirection(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
