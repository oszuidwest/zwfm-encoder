package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLastReturnsNewestWithLimitOffsetAndHasMore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	events := make([]Event, 0, 10)
	for i := range 10 {
		events = append(events, Event{
			Timestamp: time.Date(2026, 6, 12, 12, i, 0, 0, time.UTC),
			Type:      StreamStarted,
			Message:   string(rune('a' + i)),
		})
	}
	writeEvents(t, path, events)

	got, hasMore, err := ReadLast(path, 3, 2, FilterAll)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	if !hasMore {
		t.Fatal("ReadLast() hasMore = false, want true")
	}

	wantMessages := []string{"h", "g", "f"}
	assertMessages(t, got, wantMessages)
}

func TestReadLastFiltersAndSkipsMalformedLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	lines := []string{
		mustMarshal(t, &Event{Type: StreamStarted, Message: "stream-old"}),
		"not-json",
		mustMarshal(t, &Event{Type: RecorderStarted, Message: "recorder-old"}),
		mustMarshal(t, &Event{Type: SilenceStart, Message: "audio"}),
		mustMarshal(t, &Event{Type: RecorderStopped, Message: "recorder-new"}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, hasMore, err := ReadLast(path, 2, 0, FilterRecorder)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	if hasMore {
		t.Fatal("ReadLast() hasMore = true, want false")
	}

	wantMessages := []string{"recorder-new", "recorder-old"}
	assertMessages(t, got, wantMessages)
}

func TestReadLastIncludesRotatedLog(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	writeEvents(t, rotatedLogPath(path), []Event{
		{Type: StreamStarted, Message: "oldest"},
		{Type: StreamRetry, Message: "older"},
	})
	writeEvents(t, path, []Event{
		{Type: StreamStable, Message: "newer"},
		{Type: StreamStopped, Message: "newest"},
	})

	got, hasMore, err := ReadLast(path, 4, 0, FilterAll)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	if hasMore {
		t.Fatal("ReadLast() hasMore = true, want false")
	}

	wantMessages := []string{"newest", "newer", "older", "oldest"}
	assertMessages(t, got, wantMessages)
}

func TestReadLastHandlesLineLongerThanChunk(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	longMessage := strings.Repeat("x", int(tailReadChunkSize)+1024)
	writeEvents(t, path, []Event{
		{Type: StreamStarted, Message: longMessage},
		{Type: StreamStable, Message: "short"},
	})

	got, hasMore, err := ReadLast(path, 2, 0, FilterAll)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	if hasMore {
		t.Fatal("ReadLast() hasMore = true, want false")
	}

	assertMessages(t, got, []string{"short", longMessage})
}

func TestLoggerRotatesWhenSizeLimitIsReached(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := newLogger(path, 1)
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}
	defer func() {
		if err := logger.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := logger.Log(&Event{Type: StreamStarted, Message: "rotated"}); err != nil {
		t.Fatalf("Log() error = %v", err)
	}

	rotatedInfo, err := os.Stat(rotatedLogPath(path))
	if err != nil {
		t.Fatalf("stat rotated log: %v", err)
	}
	if rotatedInfo.Size() == 0 {
		t.Fatal("rotated log is empty")
	}

	activeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active log: %v", err)
	}
	if activeInfo.Size() != 0 {
		t.Fatalf("active log size = %d, want 0", activeInfo.Size())
	}

	got, hasMore, err := ReadLast(path, 1, 0, FilterAll)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	if hasMore {
		t.Fatal("ReadLast() hasMore = true, want false")
	}
	assertMessages(t, got, []string{"rotated"})
}

func writeEvents(t *testing.T, path string, events []Event) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // Test path is under t.TempDir.
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	encoder := json.NewEncoder(file)
	for i := range events {
		if err := encoder.Encode(&events[i]); err != nil {
			_ = file.Close()
			t.Fatalf("encode event: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
}

func mustMarshal(t *testing.T, event *Event) string {
	t.Helper()

	data, err := json.Marshal(&event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(data)
}

func assertMessages(t *testing.T, got []Event, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("len(events) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Message != want[i] {
			t.Fatalf("events[%d].Message = %q, want %q", i, got[i].Message, want[i])
		}
	}
}
