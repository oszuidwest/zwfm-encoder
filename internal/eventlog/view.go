package eventlog

// EventView is an event decorated with API-only classification fields.
type EventView struct {
	Event
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Reason   Reason   `json:"reason"`
}

// DecorateEvents adds serve-time classification to events without changing JSONL.
func DecorateEvents(events []Event) []EventView {
	views := make([]EventView, len(events))
	for i, event := range events {
		views[i] = EventView{
			Event:    event,
			Severity: event.Type.Severity(),
			Category: event.Type.Category(),
			Reason:   event.Type.Reason(),
		}
	}
	return views
}
