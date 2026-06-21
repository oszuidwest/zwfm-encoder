package eventlog

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// EventView is an event decorated with API-only classification fields.
type EventView struct {
	Event
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Reason   Reason   `json:"reason"`
}

// EventGroups is the historical event grouping used by the events UI.
type EventGroups struct {
	Attention    []EventGroupItem `json:"attention"`
	Resolved     []EventGroupItem `json:"resolved"`
	Activity     []EventGroupItem `json:"activity"`
	Routine      []EventGroupItem `json:"routine"`
	RoutineCount int              `json:"routineCount"`
}

// EventGroupItem is one rendered row in a historical event group.
type EventGroupItem struct {
	Key        string      `json:"key"`
	SourceKey  string      `json:"sourceKey,omitempty"`
	DedupeKey  string      `json:"dedupeKey,omitempty"`
	Category   Category    `json:"category"`
	Severity   Severity    `json:"severity"`
	Title      string      `json:"title"`
	Source     string      `json:"source"`
	StatusText string      `json:"statusText"`
	Chips      []string    `json:"chips"`
	Detail     string      `json:"detail,omitempty"`
	When       string      `json:"when,omitempty"`
	Events     []EventView `json:"events"`
	SortTs     int64       `json:"sortTs"`
	TrailLimit int         `json:"trailLimit,omitempty"`
}

type groupedEvent struct {
	id      int
	view    *EventView
	details map[string]any
}

type groupingState struct {
	openByKey map[string]*historicalIncident
	closed    []*historicalIncident
	failed    []*historicalIncident
}

type historicalIncident struct {
	key       string
	closeType EventType
	firstType EventType
	first     groupedEvent
	severity  Severity
	startTs   int64
	endTs     int64
	duration  int64
	attempts  int
	dump      bool
	events    []groupedEvent
}

var eventCloses = map[EventType]EventType{
	SilenceStart:          SilenceEnd,
	ChannelImbalanceStart: ChannelImbalanceEnd,
	StreamError:           StreamStable,
	StreamRetry:           StreamStable,
	UploadFailed:          UploadCompleted,
	UploadRetry:           UploadCompleted,
}

var eventLabels = map[EventType]string{
	StreamStarted:         "Started",
	StreamStable:          "Connected",
	StreamError:           "Error",
	StreamRetry:           "Retry",
	StreamStopped:         "Stopped",
	SilenceStart:          "Silence",
	SilenceEnd:            "Recovered",
	AudioDumpReady:        "Audio Dump",
	ChannelImbalanceStart: "Imbalance",
	ChannelImbalanceEnd:   "Balanced",
	RecorderStarted:       "Started",
	RecorderStopped:       "Stopped",
	RecorderError:         "Error",
	RecorderFile:          "New File",
	UploadQueued:          "Upload Queued",
	UploadCompleted:       "Uploaded",
	UploadFailed:          "Upload Failed",
	UploadRetry:           "Retry",
	UploadAbandoned:       "Abandoned",
	CleanupCompleted:      "Cleanup",
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

// EmptyEventGroups returns the JSON shape for an empty event history.
func EmptyEventGroups() EventGroups {
	return EventGroups{
		Attention: []EventGroupItem{},
		Resolved:  []EventGroupItem{},
		Activity:  []EventGroupItem{},
		Routine:   []EventGroupItem{},
	}
}

// GroupEvents folds historical events into attention, resolved, activity, and routine rows.
func GroupEvents(events []EventView, now time.Time) EventGroups {
	if now.IsZero() {
		now = time.Now()
	}

	grouped := make([]groupedEvent, len(events))
	for i := range events {
		grouped[i] = groupedEvent{
			id:      i,
			view:    &events[i],
			details: eventDetails(events[i].Details),
		}
	}

	asc := append([]groupedEvent(nil), grouped...)
	slices.SortStableFunc(asc, func(a, b groupedEvent) int {
		switch {
		case a.view.Timestamp.Before(b.view.Timestamp):
			return -1
		case a.view.Timestamp.After(b.view.Timestamp):
			return 1
		default:
			return a.id - b.id
		}
	})

	state := newGroupingState()
	for i := range asc {
		state.process(asc[i])
	}

	ongoing := state.ongoing()
	consumed := consumedEventIDs(state.closed, state.failed, ongoing)

	groups := EmptyEventGroups()
	for i := range grouped {
		event := grouped[i]
		if consumed[event.id] {
			continue
		}
		if event.view.Reason == ReasonRoutine {
			continue
		}
		groups.Activity = append(groups.Activity, activityEventItem(event, now))
	}

	routineEvents := []groupedEvent{}
	for i := range grouped {
		event := grouped[i]
		if !consumed[event.id] && event.view.Reason == ReasonRoutine {
			routineEvents = append(routineEvents, event)
		}
	}
	if len(routineEvents) > 0 {
		groups.Routine = append(groups.Routine, routineEventItem(routineEvents, now))
		groups.RoutineCount = len(routineEvents)
	}

	for _, incident := range ongoing {
		status := "ongoing"
		if incident.firstType == RecorderError {
			status = "unresolved"
		}
		groups.Attention = append(groups.Attention, incidentItem(incident, status, now))
	}
	for _, incident := range state.failed {
		groups.Attention = append(groups.Attention, incidentItem(incident, "failed", now))
	}
	for _, incident := range state.closed {
		groups.Resolved = append(groups.Resolved, incidentItem(incident, "resolved", now))
	}

	sortItemsDesc(groups.Attention)
	sortItemsDesc(groups.Resolved)
	return groups
}

func newGroupingState() *groupingState {
	return &groupingState{
		openByKey: map[string]*historicalIncident{},
		closed:    []*historicalIncident{},
		failed:    []*historicalIncident{},
	}
}

func (s *groupingState) process(event groupedEvent) {
	key := eventSourceKey(event)
	if open := s.openByKey[key]; open != nil && open.closeType == event.view.Type {
		open.add(event)
		s.closed = append(s.closed, open)
		delete(s.openByKey, key)
		return
	}

	if open := s.openByKey[key]; open != nil && event.view.Reason == ReasonProblem {
		open.add(event)
		return
	}

	if event.view.Type == AudioDumpReady {
		if target := latestSilenceIncident(s.closed); target != nil {
			target.dump = true
			target.add(event)
		}
		return
	}

	if event.view.Reason != ReasonProblem || isListenerHistoricalStreamProblem(event) {
		return
	}

	if event.view.Type == UploadAbandoned {
		incident := newHistoricalIncident(event, key, "")
		incident.add(event)
		s.failed = append(s.failed, incident)
		return
	}

	incident := s.openByKey[key]
	if incident == nil {
		incident = newHistoricalIncident(event, key, eventCloses[event.view.Type])
		s.openByKey[key] = incident
	}
	incident.add(event)
}

func (s *groupingState) ongoing() []*historicalIncident {
	ongoing := make([]*historicalIncident, 0, len(s.openByKey))
	for _, incident := range s.openByKey {
		ongoing = append(ongoing, incident)
	}
	return ongoing
}

func consumedEventIDs(groups ...[]*historicalIncident) map[int]bool {
	consumed := map[int]bool{}
	for _, incidents := range groups {
		for _, incident := range incidents {
			for i := range incident.events {
				consumed[incident.events[i].id] = true
			}
		}
	}
	return consumed
}

func newHistoricalIncident(event groupedEvent, key string, closeType EventType) *historicalIncident {
	startTs := eventTimeValue(event.view.Timestamp)
	return &historicalIncident{
		key:       key,
		closeType: closeType,
		firstType: event.view.Type,
		first:     event,
		severity:  event.view.Severity,
		startTs:   startTs,
		endTs:     startTs,
		events:    []groupedEvent{},
	}
}

func (i *historicalIncident) add(event groupedEvent) {
	i.events = append(i.events, event)
	i.endTs = eventTimeValue(event.view.Timestamp)
	if duration := detailInt64(event.details, "duration_ms"); duration > 0 {
		i.duration = duration
	} else {
		i.duration = max(i.duration, i.endTs-i.startTs)
	}
	if event.view.Reason == ReasonProblem {
		i.attempts++
	}
	if event.view.Severity == SeverityError {
		i.severity = SeverityError
	}
}

func latestSilenceIncident(incidents []*historicalIncident) *historicalIncident {
	for i := len(incidents) - 1; i >= 0; i-- {
		if incidents[i].firstType == SilenceStart {
			return incidents[i]
		}
	}
	return nil
}

func incidentItem(incident *historicalIncident, status string, now time.Time) EventGroupItem {
	events := incidentEventViews(incident.events)
	first := incident.first
	duration := incident.duration
	if duration == 0 {
		duration = max(0, incident.endTs-incident.startTs)
	}

	chips := []string{}
	if status == "resolved" && duration > 0 {
		chips = append(chips, formatSmartDuration(duration))
	}
	if incident.attempts > 1 {
		chips = append(chips, fmt.Sprintf("%d tries", incident.attempts))
	}
	if incident.dump {
		chips = append(chips, "audio dump")
	}
	if status == "failed" {
		if retry := detailInt64(first.details, "retry"); retry > 0 {
			chips = append(chips, fmt.Sprintf("gave up after %d", retry))
		}
	}

	severity := incident.severity
	if status == "resolved" {
		severity = SeveritySuccess
	}

	whenTs := incident.startTs
	if status == "resolved" {
		whenTs = incident.endTs
	}
	when := formatRelativeEventTime(now, whenTs)
	if status == "ongoing" {
		when = "since " + when
	}

	return EventGroupItem{
		Key:        fmt.Sprintf("incident:%s:%s:%d", status, incident.key, incident.startTs),
		SourceKey:  incident.key,
		DedupeKey:  eventDedupeKey(first),
		Category:   first.view.Category,
		Severity:   severity,
		Title:      incidentTitle(first.view.Type),
		Source:     eventSource(first),
		StatusText: statusText(status),
		Chips:      chips,
		Detail:     firstEventError(incident.events),
		When:       when,
		Events:     events,
		SortTs:     incident.startTs,
	}
}

func activityEventItem(event groupedEvent, now time.Time) EventGroupItem {
	return EventGroupItem{
		Key:        eventRowKey(event),
		SourceKey:  eventSourceKey(event),
		DedupeKey:  eventDedupeKey(event),
		Category:   event.view.Category,
		Severity:   event.view.Severity,
		Title:      activityTitle(event),
		Source:     eventSource(event),
		StatusText: activityStatusText(event),
		Chips:      []string{},
		Detail:     eventDetail(event),
		When:       formatRelativeEventTime(now, eventTimeValue(event.view.Timestamp)),
		Events:     []EventView{*event.view},
		SortTs:     eventTimeValue(event.view.Timestamp),
	}
}

func routineEventItem(events []groupedEvent, now time.Time) EventGroupItem {
	chips := []string{}
	files := countEvents(events, RecorderFile)
	queued := countEvents(events, UploadQueued)
	uploaded := countEvents(events, UploadCompleted)
	cleanups := countEvents(events, CleanupCompleted)
	if files > 0 {
		chips = append(chips, pluralCount(files, "file"))
	}
	if uploaded > 0 {
		chips = append(chips, pluralCount(uploaded, "upload"))
	} else if queued > 0 {
		chips = append(chips, fmt.Sprintf("%d queued", queued))
	}
	if cleanups > 0 {
		chips = append(chips, pluralCount(cleanups, "cleanup"))
	}

	first := events[0]
	source := "zwfm-hourly"
	for i := range events {
		if name := detailString(events[i].details, "recorder_name"); name != "" {
			source = name
			break
		}
	}

	return EventGroupItem{
		Key:        fmt.Sprintf("routine:%d:%s", len(events), first.view.Timestamp.Format(time.RFC3339Nano)),
		Category:   CategoryRecorder,
		Severity:   SeveritySuccess,
		Title:      "Hourly recording",
		Source:     source,
		StatusText: "Normal",
		Chips:      chips,
		When:       "last " + formatRelativeEventTime(now, eventTimeValue(first.view.Timestamp)),
		Events:     groupedEventViews(events),
		SortTs:     eventTimeValue(first.view.Timestamp),
		TrailLimit: 8,
	}
}

func eventSourceKey(event groupedEvent) string {
	switch event.view.Category {
	case CategoryStream:
		if event.view.StreamID != "" {
			return "stream:" + event.view.StreamID
		}
		if name := detailString(event.details, "stream_name"); name != "" {
			return "stream:" + name
		}
		return fmt.Sprintf("stream-event:%d", event.id)
	case CategoryAudio:
		if strings.HasPrefix(string(event.view.Type), "channel_imbalance_") {
			return "audio:imbalance"
		}
		return "audio:silence"
	case CategoryRecorder:
		name := detailString(event.details, "recorder_name")
		if isUploadEvent(event.view.Type) {
			file := firstNonEmpty(
				detailString(event.details, "s3_key"),
				detailString(event.details, "filename"),
			)
			if name != "" || file != "" {
				return "recorder:" + name + ":" + file
			}
			return fmt.Sprintf("recorder-upload-event:%d", event.id)
		}
		if name != "" {
			return recorderStatusSourceKey(name)
		}
		return fmt.Sprintf("recorder-event:%s:%d", event.view.Type, event.id)
	default:
		return fmt.Sprintf("unknown:%s:%d", event.view.Type, event.id)
	}
}

func eventDedupeKey(event groupedEvent) string {
	if event.view.Category == CategoryRecorder && isUploadEvent(event.view.Type) {
		if name := detailString(event.details, "recorder_name"); name != "" {
			return recorderUploadSourceKey(name)
		}
	}
	return eventSourceKey(event)
}

func recorderStatusSourceKey(name string) string {
	return "recorder:" + name + ":"
}

func recorderUploadSourceKey(name string) string {
	return "recorder-upload:" + name
}

func isListenerHistoricalStreamProblem(event groupedEvent) bool {
	return event.view.Category == CategoryStream &&
		event.view.Reason == ReasonProblem &&
		detailString(event.details, "mode") == "listener"
}

func eventSource(event groupedEvent) string {
	switch event.view.Category {
	case CategoryAudio:
		return "Audio"
	case CategoryRecorder:
		if name := detailString(event.details, "recorder_name"); name != "" {
			return name
		}
		return "Recorder"
	case CategoryStream:
		if name := detailString(event.details, "stream_name"); name != "" {
			return name
		}
		if event.view.StreamID != "" {
			return event.view.StreamID
		}
		return "Stream"
	default:
		return "System"
	}
}

func incidentTitle(t EventType) string {
	switch t {
	case SilenceStart:
		return "Silence on input"
	case ChannelImbalanceStart:
		return "Channel imbalance"
	case StreamError, StreamRetry:
		return "Stream disconnected"
	case UploadFailed, UploadRetry:
		return "Upload failed"
	case UploadAbandoned:
		return "Upload abandoned"
	case RecorderError:
		return "Recorder error"
	default:
		return eventLabel(t)
	}
}

func activityTitle(event groupedEvent) string {
	if event.view.Type == StreamStarted && strings.HasPrefix(event.view.Message, "Listening on") {
		return "Listener started"
	}
	switch event.view.Type {
	case StreamStarted:
		return "Stream started"
	case StreamStable:
		return "Stream connected"
	case StreamStopped:
		return "Stream stopped"
	case RecorderStarted:
		return "Recorder started"
	case RecorderStopped:
		return "Recorder stopped"
	default:
		return eventLabel(event.view.Type)
	}
}

func activityStatusText(event groupedEvent) string {
	if event.view.Type == StreamStarted && strings.HasPrefix(event.view.Message, "Listening on") {
		return "Listening"
	}
	switch event.view.Type {
	case StreamStarted:
		return "Started"
	case StreamStable:
		return "Connected"
	case StreamStopped:
		return "Stopped"
	case RecorderStarted:
		return "Started"
	case RecorderStopped:
		return "Stopped"
	case SilenceEnd, ChannelImbalanceEnd:
		return "Recovered"
	default:
		return eventLabel(event.view.Type)
	}
}

func statusText(status string) string {
	switch status {
	case "failed":
		return "Failed"
	case "resolved":
		return "Resolved"
	case "unresolved":
		return "Unresolved"
	default:
		return "Ongoing"
	}
}

func eventLabel(t EventType) string {
	if label, ok := eventLabels[t]; ok {
		return label
	}
	return string(t)
}

func eventDetail(event groupedEvent) string {
	details := event.details
	switch event.view.Type {
	case StreamError:
		return firstNonEmpty(detailString(details, "error"), "Unknown error")
	case StreamRetry:
		return retryErrorDetail(details, "")
	case SilenceStart:
		left, okLeft := detailFloat(details, "level_left_db")
		right, okRight := detailFloat(details, "level_right_db")
		if okLeft && okRight {
			return fmt.Sprintf("L: %.1fdB  R: %.1fdB", left, right)
		}
	case SilenceEnd, ChannelImbalanceEnd:
		if duration := detailInt64(details, "duration_ms"); duration > 0 {
			return "Duration: " + formatSmartDuration(duration)
		}
	case ChannelImbalanceStart:
		left, okLeft := detailFloat(details, "level_left_db")
		right, okRight := detailFloat(details, "level_right_db")
		diff, okDiff := detailFloat(details, "imbalance_db")
		if okLeft && okRight && okDiff {
			return fmt.Sprintf("L: %.1fdB  R: %.1fdB (%.1fdB diff)", left, right, diff)
		}
	case AudioDumpReady:
		if dumpErr := detailString(details, "dump_error"); dumpErr != "" {
			return "Error: " + dumpErr
		}
		return detailString(details, "dump_filename")
	case RecorderError, UploadFailed:
		return firstNonEmpty(detailString(details, "error"), "Unknown error")
	case UploadRetry, UploadAbandoned:
		return retryErrorDetail(details, "Unknown error")
	case RecorderFile, UploadQueued, UploadCompleted:
		codec := strings.ToUpper(detailString(details, "codec"))
		return joinNonEmpty(" - ", detailString(details, "filename"), codec)
	case CleanupCompleted:
		count := detailInt64(details, "files_deleted")
		storage := detailString(details, "storage_type")
		return fmt.Sprintf("%d files deleted (%s)", count, storage)
	case RecorderStarted, RecorderStopped:
		codec := strings.ToUpper(detailString(details, "codec"))
		mode := recorderStorageLabel(detailString(details, "storage_mode"))
		return joinNonEmpty(" - ", codec, mode)
	}
	return detailString(details, "stream_name")
}

func firstEventError(events []groupedEvent) string {
	for _, event := range events {
		if errMsg := detailString(event.details, "error"); errMsg != "" {
			return errMsg
		}
	}
	if len(events) > 0 {
		return firstNonEmpty(eventDetail(events[0]), events[0].view.Message)
	}
	return ""
}

func retryErrorDetail(details map[string]any, fallback string) string {
	parts := []string{}
	if retry := detailInt64(details, "retry"); retry > 0 {
		parts = append(parts, fmt.Sprintf("Retry #%d", retry))
	}
	if errMsg := detailString(details, "error"); errMsg != "" {
		parts = append(parts, errMsg)
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, " - ")
}

func recorderStorageLabel(mode string) string {
	switch mode {
	case "both":
		return "Local + S3"
	case "s3":
		return "S3"
	case "local":
		return "Local"
	default:
		return ""
	}
}

func countEvents(events []groupedEvent, eventType EventType) int {
	count := 0
	for i := range events {
		if events[i].view.Type == eventType {
			count++
		}
	}
	return count
}

func groupedEventViews(events []groupedEvent) []EventView {
	views := make([]EventView, len(events))
	for i := range events {
		views[i] = *events[i].view
	}
	return views
}

func incidentEventViews(events []groupedEvent) []EventView {
	ordered := append([]groupedEvent(nil), events...)
	slices.SortStableFunc(ordered, func(a, b groupedEvent) int {
		switch {
		case a.view.Timestamp.Before(b.view.Timestamp):
			return -1
		case a.view.Timestamp.After(b.view.Timestamp):
			return 1
		default:
			return a.id - b.id
		}
	})
	return groupedEventViews(ordered)
}

func sortItemsDesc(items []EventGroupItem) {
	slices.SortStableFunc(items, func(a, b EventGroupItem) int {
		switch {
		case a.SortTs > b.SortTs:
			return -1
		case a.SortTs < b.SortTs:
			return 1
		default:
			return strings.Compare(a.Key, b.Key)
		}
	})
}

func eventRowKey(event groupedEvent) string {
	return fmt.Sprintf(
		"event:%s:%s:%s:%d",
		event.view.Type,
		event.view.Timestamp.Format(time.RFC3339Nano),
		event.view.StreamID,
		event.id,
	)
}

func isUploadEvent(t EventType) bool {
	switch t {
	case UploadQueued, UploadCompleted, UploadFailed, UploadRetry, UploadAbandoned:
		return true
	default:
		return false
	}
}

func eventTimeValue(timestamp time.Time) int64 {
	if timestamp.IsZero() {
		return 0
	}
	return timestamp.UnixMilli()
}

func formatRelativeEventTime(now time.Time, timestampMs int64) string {
	if timestampMs == 0 {
		return ""
	}
	diff := max(int64(0), now.UnixMilli()-timestampMs)
	switch {
	case diff < int64(time.Minute/time.Millisecond):
		return "just now"
	case diff < int64(time.Hour/time.Millisecond):
		return fmt.Sprintf("%dm ago", diff/int64(time.Minute/time.Millisecond))
	case diff < int64((24*time.Hour)/time.Millisecond):
		return fmt.Sprintf("%dh ago", diff/int64(time.Hour/time.Millisecond))
	default:
		return fmt.Sprintf("%dd ago", diff/int64((24*time.Hour)/time.Millisecond))
	}
}

func formatSmartDuration(ms int64) string {
	if ms < int64(time.Second/time.Millisecond) {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < int64(time.Minute/time.Millisecond) {
		seconds := float64(ms) / float64(time.Second/time.Millisecond)
		if seconds < 10 {
			return fmt.Sprintf("%.1fs", seconds)
		}
		return fmt.Sprintf("%.0fs", math.Round(seconds))
	}
	minutes := ms / int64(time.Minute/time.Millisecond)
	seconds := int64(math.Round(float64(ms%int64(time.Minute/time.Millisecond)) / float64(time.Second/time.Millisecond)))
	if seconds > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%dm", minutes)
}

func eventDetails(detailsValue any) map[string]any {
	if detailsValue == nil {
		return map[string]any{}
	}
	if details, ok := detailsValue.(map[string]any); ok {
		return details
	}
	data, err := json.Marshal(detailsValue)
	if err != nil {
		return map[string]any{}
	}
	details := map[string]any{}
	if err := json.Unmarshal(data, &details); err != nil {
		return map[string]any{}
	}
	return details
}

func detailString(details map[string]any, key string) string {
	if value, ok := details[key].(string); ok {
		return value
	}
	return ""
}

func detailInt64(details map[string]any, key string) int64 {
	switch value := details[key].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err == nil {
			return parsed
		}
	}
	return 0
}

func detailFloat(details map[string]any, key string) (float64, bool) {
	switch value := details[key].(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func pluralCount(count int, singular string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", count, singular)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func joinNonEmpty(sep string, values ...string) string {
	parts := []string{}
	for _, value := range values {
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, sep)
}
