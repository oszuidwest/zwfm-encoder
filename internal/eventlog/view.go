package eventlog

// EventView is an event plus serve-time display metadata.
type EventView struct {
	Event
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Reason   Reason   `json:"reason"`
	Label    string   `json:"label"`
	Detail   string   `json:"detail,omitempty"`
}

// DecorateEvents adds display metadata without changing JSONL records.
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
