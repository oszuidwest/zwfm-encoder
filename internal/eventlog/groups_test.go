package eventlog

import (
	"encoding/json"
	"fmt"
	"maps"
	"testing"
	"time"
)

func TestGroupEventsPairsProblemsAndPartitionsEvents(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 8, CleanupCompleted, nil),
		testEvent(base, 7, UploadCompleted, map[string]any{
			"recorder_name": "hourly",
			"filename":      "a.mp3",
		}),
		testEvent(base, 6, UploadFailed, map[string]any{
			"recorder_name": "hourly",
			"filename":      "a.mp3",
			"error":         "s3 unavailable",
		}),
		testEvent(base, 5, AudioDumpReady, map[string]any{
			"dump_filename": "silence.mp3",
		}),
		testEvent(base, 4, SilenceEnd, map[string]any{
			"duration_ms": 3000,
		}),
		testEvent(base, 3, SilenceStart, map[string]any{
			"level_left_db":  -64.2,
			"level_right_db": -63.8,
		}),
		testEvent(base, 2, StreamStarted, map[string]any{
			"stream_name": "Main",
			"mode":        "caller",
		}),
		testEvent(base, 1, ChannelImbalanceEnd, map[string]any{
			"duration_ms": 1200,
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Attention) != 0 {
		t.Fatalf("attention len = %d, want 0", len(groups.Attention))
	}
	if len(groups.Resolved) != 2 {
		t.Fatalf("resolved len = %d, want 2", len(groups.Resolved))
	}
	if got := groups.Resolved[0].Events[0].Type; got != UploadFailed {
		t.Fatalf("first resolved incident starts with %q, want %q", got, UploadFailed)
	}
	if got := groups.Resolved[1].Events[len(groups.Resolved[1].Events)-1].Type; got != AudioDumpReady {
		t.Fatalf("silence incident last event = %q, want %q", got, AudioDumpReady)
	}
	if len(groups.Activity) != 2 {
		t.Fatalf("activity len = %d, want 2", len(groups.Activity))
	}
	if groups.RoutineCount != 1 {
		t.Fatalf("routine count = %d, want 1", groups.RoutineCount)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsTreatsCallerAndListenerStreamProblemsAsIncidents(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 2, StreamRetry, map[string]any{
			"stream_name": "Listener",
			"mode":        "listener",
		}),
		testEvent(base, 1, StreamRetry, map[string]any{
			"stream_name": "Caller",
			"mode":        "caller",
		}),
	)
	events[0].StreamID = "listener"
	events[1].StreamID = "caller"

	groups := GroupEvents(events)
	if len(groups.Attention) != 2 {
		t.Fatalf("attention len = %d, want 2", len(groups.Attention))
	}
	items := itemsBySourceKey(groups.Attention)
	if _, ok := items["stream:caller"]; !ok {
		t.Fatal("caller stream problem missing from attention")
	}
	if _, ok := items["stream:listener"]; !ok {
		t.Fatal("listener stream problem missing from attention")
	}
	if len(groups.Activity) != 0 {
		t.Fatalf("activity len = %d, want 0", len(groups.Activity))
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsUploadDedupeKeyDoesNotCollapseHistoricalFiles(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 2, UploadFailed, map[string]any{
			"recorder_name": "hourly",
			"filename":      "b.mp3",
		}),
		testEvent(base, 1, UploadFailed, map[string]any{
			"recorder_name": "hourly",
			"filename":      "a.mp3",
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Attention) != 2 {
		t.Fatalf("attention len = %d, want 2", len(groups.Attention))
	}
	if groups.Attention[0].SourceKey == groups.Attention[1].SourceKey {
		t.Fatalf("source keys collided: %q", groups.Attention[0].SourceKey)
	}
	for i := range groups.Attention {
		if groups.Attention[i].DedupeKey != "recorder-upload:hourly" {
			t.Fatalf("dedupe key = %q, want recorder-upload:hourly", groups.Attention[i].DedupeKey)
		}
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsAvoidsEmptyRecorderNameUploadCollision(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 2, UploadFailed, map[string]any{
			"filename": "b.mp3",
		}),
		testEvent(base, 1, UploadFailed, map[string]any{
			"filename": "a.mp3",
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Attention) != 2 {
		t.Fatalf("attention len = %d, want 2", len(groups.Attention))
	}
	if groups.Attention[0].SourceKey == groups.Attention[1].SourceKey {
		t.Fatalf("empty-name source keys collided: %q", groups.Attention[0].SourceKey)
	}
	for i := range groups.Attention {
		if groups.Attention[i].DedupeKey != groups.Attention[i].SourceKey {
			t.Fatalf(
				"empty-name dedupe key = %q, want source key %q",
				groups.Attention[i].DedupeKey,
				groups.Attention[i].SourceKey,
			)
		}
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsKeepsOrphanRecoveryInActivity(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 1, SilenceEnd, map[string]any{
			"duration_ms": 5000,
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Resolved) != 0 || len(groups.Attention) != 0 {
		t.Fatalf("orphan recovery grouped as incident: attention=%d resolved=%d", len(groups.Attention), len(groups.Resolved))
	}
	if len(groups.Activity) != 1 {
		t.Fatalf("activity len = %d, want 1", len(groups.Activity))
	}
	if got := groups.Activity[0].Events[0].Type; got != SilenceEnd {
		t.Fatalf("activity event = %q, want %q", got, SilenceEnd)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsKeepsOrphanAudioDumpInActivity(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 1, AudioDumpReady, map[string]any{
			"dump_filename": "orphan.mp3",
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Resolved) != 0 || len(groups.Attention) != 0 {
		t.Fatalf("orphan dump grouped as incident: attention=%d resolved=%d", len(groups.Attention), len(groups.Resolved))
	}
	if len(groups.Activity) != 1 {
		t.Fatalf("activity len = %d, want 1", len(groups.Activity))
	}
	if got := groups.Activity[0].Events[0].Type; got != AudioDumpReady {
		t.Fatalf("activity event = %q, want %q", got, AudioDumpReady)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsHandlesZeroTimestamp(t *testing.T) {
	t.Parallel()

	events := DecorateEvents([]Event{
		{
			Type: StreamStarted,
			Details: map[string]any{
				"stream_name": "Main",
				"mode":        "caller",
			},
		},
	})

	groups := GroupEvents(events)
	if len(groups.Activity) != 1 {
		t.Fatalf("activity len = %d, want 1", len(groups.Activity))
	}
	if groups.Activity[0].SortTs != 0 {
		t.Fatalf("sortTs = %d, want 0", groups.Activity[0].SortTs)
	}
	if groups.Activity[0].Key == "" {
		t.Fatal("key is empty")
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsLabelsListenerStartActivity(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := DecorateEvents([]Event{
		{
			Timestamp: base.Add(2 * time.Second),
			Type:      StreamStarted,
			StreamID:  "listener",
			Message:   "Listening on 0.0.0.0:9000",
			Details: map[string]any{
				"stream_name": "Listener",
				"mode":        "listener",
			},
		},
		{
			Timestamp: base.Add(time.Second),
			Type:      StreamStarted,
			StreamID:  "caller",
			Message:   "Connecting to stream.example.com:9000",
			Details: map[string]any{
				"stream_name": "Caller",
				"mode":        "caller",
			},
		},
	})

	groups := GroupEvents(events)
	if len(groups.Activity) != 2 {
		t.Fatalf("activity len = %d, want 2", len(groups.Activity))
	}

	items := map[string]EventGroupItem{}
	for i := range groups.Activity {
		items[groups.Activity[i].SourceKey] = groups.Activity[i]
	}
	if got := items["stream:listener"].Title; got != "Listener started" {
		t.Fatalf("listener title = %q, want Listener started", got)
	}
	if got := items["stream:listener"].StatusText; got != "Listening" {
		t.Fatalf("listener status = %q, want Listening", got)
	}
	if got := items["stream:caller"].Title; got != "Stream started" {
		t.Fatalf("caller title = %q, want Stream started", got)
	}
	if got := items["stream:caller"].StatusText; got != "Started" {
		t.Fatalf("caller status = %q, want Started", got)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsLiveDedupeKeyContract(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := DecorateEvents([]Event{
		{
			Timestamp: base.Add(time.Second),
			Type:      StreamRetry,
			StreamID:  "s1",
			Details: map[string]any{
				"mode": "caller",
			},
		},
		{
			Timestamp: base.Add(2 * time.Second),
			Type:      SilenceStart,
		},
		{
			Timestamp: base.Add(3 * time.Second),
			Type:      ChannelImbalanceStart,
		},
		{
			Timestamp: base.Add(4 * time.Second),
			Type:      RecorderError,
			Details: map[string]any{
				"recorder_name": "hourly",
			},
		},
		{
			Timestamp: base.Add(5 * time.Second),
			Type:      UploadFailed,
			Details: map[string]any{
				"recorder_name": "hourly",
				"filename":      "hourly.mp3",
			},
		},
	})

	groups := GroupEvents(events)
	got := map[string]string{}
	for _, item := range groups.Attention {
		switch item.Events[0].Type {
		case StreamRetry:
			got["stream"] = item.DedupeKey
		case SilenceStart:
			got["silence"] = item.DedupeKey
		case ChannelImbalanceStart:
			got["imbalance"] = item.DedupeKey
		case RecorderError:
			got["recorder"] = item.DedupeKey
		case UploadFailed:
			got["upload"] = item.DedupeKey
		}
	}

	want := map[string]string{
		"stream":    "stream:s1",
		"silence":   "audio:silence",
		"imbalance": "audio:imbalance",
		"recorder":  "recorder:hourly:",
		"upload":    "recorder-upload:hourly",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("dedupe keys = %v, want %v", got, want)
	}
}

func TestGroupEventsRecorderErrorIsUnresolved(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 1, RecorderError, map[string]any{
			"recorder_name": "hourly",
			"error":         "disk full",
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Attention) != 1 {
		t.Fatalf("attention len = %d, want 1", len(groups.Attention))
	}
	item := groups.Attention[0]
	if item.StatusText != "Unresolved" {
		t.Fatalf("status = %q, want Unresolved", item.StatusText)
	}
	if item.Detail != "disk full" {
		t.Fatalf("detail = %q, want disk full", item.Detail)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsUploadAbandonedIsFailed(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 1, UploadAbandoned, map[string]any{
			"recorder_name": "hourly",
			"filename":      "hourly.mp3",
			"error":         "s3 offline",
			"retry":         4,
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Attention) != 1 {
		t.Fatalf("attention len = %d, want 1", len(groups.Attention))
	}
	item := groups.Attention[0]
	if item.StatusText != "Failed" {
		t.Fatalf("status = %q, want Failed", item.StatusText)
	}
	if !containsString(item.Chips, "gave up after 4") {
		t.Fatalf("chips = %v, want gave up after 4", item.Chips)
	}
	assertPartition(t, events, &groups)
}

func TestGroupEventsLabelsMultipleRoutineRecorders(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 2, RecorderFile, map[string]any{
			"recorder_name": "hourly-a",
		}),
		testEvent(base, 1, RecorderFile, map[string]any{
			"recorder_name": "hourly-b",
		}),
	)

	groups := GroupEvents(events)
	if len(groups.Routine) != 1 {
		t.Fatalf("routine len = %d, want 1", len(groups.Routine))
	}
	if got := groups.Routine[0].Source; got != "2 recorders" {
		t.Fatalf("routine source = %q, want 2 recorders", got)
	}
	assertPartition(t, events, &groups)
}

func TestDetailNumberCoercionHandlesJSONNumber(t *testing.T) {
	t.Parallel()

	details := map[string]any{
		"duration_ms":  json.Number("1500"),
		"imbalance_db": json.Number("12.5"),
	}
	if got := detailInt64(details, "duration_ms"); got != 1500 {
		t.Fatalf("duration_ms = %d, want 1500", got)
	}
	if got, ok := detailFloat(details, "imbalance_db"); !ok || got != 12.5 {
		t.Fatalf("imbalance_db = (%v, %v), want (12.5, true)", got, ok)
	}
}

func testEvent(base time.Time, second int, eventType EventType, details map[string]any) Event {
	return Event{
		Timestamp: base.Add(time.Duration(second) * time.Second),
		Type:      eventType,
		Details:   details,
	}
}

func decorateNewestFirst(events ...Event) []EventView {
	return DecorateEvents(events)
}

func assertPartition(t *testing.T, events []EventView, groups *EventGroups) {
	t.Helper()
	want := map[string]int{}
	for i := range events {
		want[eventTestKey(&events[i])]++
	}
	got := map[string]int{}
	for _, section := range [][]EventGroupItem{
		groups.Attention,
		groups.Resolved,
		groups.Activity,
		groups.Routine,
	} {
		for i := range section {
			for j := range section[i].Events {
				got[eventTestKey(&section[i].Events[j])]++
			}
		}
	}
	for key, wantCount := range want {
		if got[key] != wantCount {
			t.Fatalf("event %s grouped %d times, want %d", key, got[key], wantCount)
		}
	}
	for key, gotCount := range got {
		if want[key] != gotCount {
			t.Fatalf("unexpected grouped event %s count %d", key, gotCount)
		}
	}
}

func itemsBySourceKey(items []EventGroupItem) map[string]EventGroupItem {
	bySource := make(map[string]EventGroupItem, len(items))
	for i := range items {
		bySource[items[i].SourceKey] = items[i]
	}
	return bySource
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func eventTestKey(event *EventView) string {
	return fmt.Sprintf("%s/%d", event.Type, event.Timestamp.UnixNano())
}

var benchmarkGroups EventGroups

func BenchmarkDecorateAndGroupEvents(b *testing.B) {
	events := benchmarkEventHistory(500)
	b.ReportAllocs()
	for b.Loop() {
		views := DecorateEvents(events)
		benchmarkGroups = GroupEvents(views)
	}
}

func benchmarkEventHistory(total int) []Event {
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	events := make([]Event, total)
	for i := range total {
		events[total-1-i] = benchmarkEvent(base, i)
	}
	return events
}

func benchmarkEvent(base time.Time, i int) Event {
	ts := base.Add(time.Duration(i) * time.Second)
	streamID := fmt.Sprintf("stream-%02d", i%5)
	recorder := fmt.Sprintf("hourly-%02d", i%4)
	filename := fmt.Sprintf("rec-%03d.mp3", i/12)
	event := Event{
		Timestamp: ts,
		StreamID:  streamID,
		Details: map[string]any{
			"stream_name":    streamID,
			"mode":           "caller",
			"error":          "connection lost",
			"retry":          1,
			"level_left_db":  -62.5,
			"level_right_db": -63.1,
			"duration_ms":    int64(1400),
			"recorder_name":  recorder,
			"filename":       filename,
			"s3_key":         filename,
			"codec":          "mp3",
			"storage_mode":   "both",
		},
	}

	switch i % 12 {
	case 0:
		event.Type = StreamStarted
		event.Message = "Connecting"
	case 1:
		event.Type = StreamError
	case 2:
		event.Type = StreamRetry
	case 3:
		event.Type = StreamStable
	case 4:
		event.Type = SilenceStart
		event.StreamID = ""
	case 5:
		event.Type = SilenceEnd
		event.StreamID = ""
	case 6:
		event.Type = RecorderStarted
		event.StreamID = ""
	case 7:
		event.Type = RecorderFile
		event.StreamID = ""
	case 8:
		event.Type = UploadQueued
		event.StreamID = ""
	case 9:
		event.Type = UploadFailed
		event.StreamID = ""
	case 10:
		event.Type = UploadRetry
		event.StreamID = ""
	default:
		event.Type = UploadCompleted
		event.StreamID = ""
	}
	return event
}
