package eventlog

import (
	"testing"
	"time"
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
