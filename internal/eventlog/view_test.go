package eventlog

import "testing"

func TestDecorateEventsAddsDisplayFields(t *testing.T) {
	t.Parallel()

	events := DecorateEvents([]Event{
		{
			Type: SilenceStart,
			Details: map[string]any{
				"level_left_db": -64.2,
			},
		},
		{
			Type: ChannelImbalanceStart,
			Details: map[string]any{
				"level_right_db": -18.1,
				"imbalance_db":   12.5,
			},
		},
		{
			Type:    UploadAbandoned,
			Details: map[string]any{},
		},
		{
			Type: UploadAbandoned,
			Details: map[string]any{
				"retry": 24,
				"error": "final timeout",
			},
		},
	})

	tests := []struct {
		name       string
		eventIndex int
		wantLabel  string
		wantDetail string
	}{
		{
			name:       "partial silence details",
			eventIndex: 0,
			wantLabel:  "Silence",
			wantDetail: "",
		},
		{
			name:       "partial imbalance details",
			eventIndex: 1,
			wantLabel:  "Imbalance",
			wantDetail: "",
		},
		{
			name:       "unknown abandoned upload",
			eventIndex: 2,
			wantLabel:  "Abandoned",
			wantDetail: "Unknown error",
		},
		{
			name:       "abandoned upload retry detail",
			eventIndex: 3,
			wantLabel:  "Abandoned",
			wantDetail: "Retry #24 - final timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event := events[tt.eventIndex]
			if event.Label != tt.wantLabel {
				t.Fatalf("Label = %q, want %q", event.Label, tt.wantLabel)
			}
			if event.Detail != tt.wantDetail {
				t.Fatalf("Detail = %q, want %q", event.Detail, tt.wantDetail)
			}
		})
	}
}
