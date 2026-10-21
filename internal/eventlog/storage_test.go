package eventlog

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestStorageIncidentsGroupedByPathAndIndependentOfUploads(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 4, RecordingStorageRestored, map[string]any{"path": "/spool", "available_bytes": 2147483648, "threshold_bytes": 1073741824}),
		testEvent(base, 3, UploadAbandoned, map[string]any{"recorder_name": "/spool", "filename": "audio.mp3", "error": "S3 down"}),
		testEvent(base, 2, RecordingStorageLow, map[string]any{"path": "/archive", "error": "offline"}),
		testEvent(base, 1, RecordingStorageLow, map[string]any{"path": "/spool", "available_bytes": 0, "threshold_bytes": 1073741824}),
	)
	groups := GroupEvents(events, nil)
	if len(groups.Attention) != 2 || len(groups.Resolved) != 1 || groups.Resolved[0].Source != "/spool" {
		t.Fatalf("storage recovery closed the wrong incident: %+v", groups)
	}
	for _, event := range events {
		if event.Type == RecordingStorageRestored && (event.Label != "Storage Restored" || event.Detail == "" || event.Category != CategoryRecorder) {
			t.Fatalf("storage metadata missing: %+v", event)
		}
	}
	assertPartition(t, events, &groups)
}

func TestStorageRecoveryOfUnmonitoredPathSaysSo(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(testEvent(base, 1, RecordingStorageRestored, map[string]any{"path": "/old"}))
	if events[0].Detail != "Path no longer monitored" {
		t.Fatalf("recovery of a path that left monitoring reads %q", events[0].Detail)
	}
}

func TestStorageKindChangeStaysOneIncident(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	events := decorateNewestFirst(
		testEvent(base, 3, RecordingStorageRestored, map[string]any{"path": "/spool", "available_bytes": int64(3 << 30), "threshold_bytes": 1073741824}),
		testEvent(base, 2, RecordingStorageLow, map[string]any{"path": "/spool", "error": "offline"}),
		testEvent(base, 1, RecordingStorageLow, map[string]any{"path": "/spool", "available_bytes": 0, "threshold_bytes": 1073741824, "low": true}),
	)
	groups := GroupEvents(events, nil)
	if len(groups.Attention) != 0 || len(groups.Resolved) != 1 {
		t.Fatalf("a change from low to unavailable split the incident: %+v", groups)
	}
	if chips := groups.Resolved[0].Chips; slices.ContainsFunc(chips, func(c string) bool { return strings.HasSuffix(c, "tries") }) {
		t.Fatalf("storage incident counts a change of kind as a retry: %v", chips)
	}
	assertPartition(t, events, &groups)
}

func TestLatestStorageEventsReadsAllRetainedHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	writeEvents(t, util.RotatedPath(path), []Event{
		{Type: RecordingStorageLow, Message: "a-open", Details: map[string]any{"path": "/a", "low": true}},
		{Type: RecordingStorageLow, Message: "b-old", Details: map[string]any{"path": "/b", "low": true}},
	})
	current := []Event{
		{Type: RecordingStorageRestored, Message: "b-new", Details: map[string]any{"path": "/b"}},
		{Type: StreamStarted, Message: "unrelated"},
	}
	for range MaxReadLimit + 1 { // More than one ReadLast page of another path.
		current = append(current, Event{Type: RecordingStorageLow, Message: "c", Details: map[string]any{"path": "/c", "error": "offline"}})
	}
	writeEvents(t, path, current)
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	latest, err := logger.LatestStorageEvents()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range latest {
		got = append(got, e.Message)
	}
	if !slices.Equal(got, []string{"c", "b-new", "a-open"}) {
		t.Fatalf("latest storage events = %v, want c, b-new, a-open", got)
	}
}
