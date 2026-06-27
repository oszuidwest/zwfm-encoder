package notify

import "testing"

func TestFormatZabbixChannelImbalanceValues(t *testing.T) {
	t.Parallel()
	start := formatZabbixChannelImbalanceStartValue(ChannelImbalanceData{
		LevelL:      -6,
		LevelR:      -30,
		BalanceDB:   24,
		ImbalanceDB: 24,
		ThresholdDB: 12,
	})
	wantStart := "event=CHANNEL_IMBALANCE level_l=-6.0 level_r=-30.0 balance_db=24.0 imbalance_db=24.0 threshold=12.0"
	if start != wantStart {
		t.Fatalf("start value = %q, want %q", start, wantStart)
	}

	end := formatZabbixChannelImbalanceEndValue(ChannelImbalanceData{
		LevelL:      -8,
		LevelR:      -8,
		BalanceDB:   0,
		ImbalanceDB: 0,
		ThresholdDB: 12,
		DurationMs:  16000,
	})
	wantEnd := "event=CHANNEL_BALANCED duration_ms=16000 level_l=-8.0 level_r=-8.0 balance_db=0.0 imbalance_db=0.0 threshold=12.0"
	if end != wantEnd {
		t.Fatalf("end value = %q, want %q", end, wantEnd)
	}
}
