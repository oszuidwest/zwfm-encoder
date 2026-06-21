package eventlog

// EventView is an event decorated with API-only display metadata.
type EventView struct {
	Event
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Reason   Reason   `json:"reason"`
	Label    string   `json:"label"`
	Detail   string   `json:"detail,omitempty"`
}

// DecorateEvents adds serve-time display metadata to events without changing JSONL.
func DecorateEvents(events []Event) []EventView {
	views := make([]EventView, len(events))
	for i, event := range events {
		details := eventDetails(event.Details)
		views[i] = EventView{
			Event:    event,
			Severity: event.Type.Severity(),
			Category: event.Type.Category(),
			Reason:   event.Type.Reason(),
			Label:    eventLabel(event.Type),
			Detail:   eventDetailFor(event.Type, details),
		}
	}
	return views
}
