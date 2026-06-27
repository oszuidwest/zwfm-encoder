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

func TestLoggerSeqIncrementsOnEachWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	defer func() {
		if err := logger.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()
	if got := logger.Seq(); got != 0 {
		t.Fatalf("Seq() before any write = %d, want 0", got)
	}
	if err := logger.Log(&Event{Type: StreamStarted, Message: "first"}); err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if got := logger.Seq(); got != 1 {
		t.Fatalf("Seq() after first write = %d, want 1", got)
	}
	if err := logger.Log(&Event{Type: StreamStopped, Message: "second"}); err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if got := logger.Seq(); got != 2 {
		t.Fatalf("Seq() after second write = %d, want 2", got)
	}
}

func TestLoggerSeqDoesNotIncrementOnFailedWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	// Close the underlying file so the encode write fails before the counter is bumped.
	if err := logger.file.Close(); err != nil {
		t.Fatalf("closing underlying file: %v", err)
	}
	if err := logger.Log(&Event{Type: StreamStarted, Message: "doomed"}); err == nil {
		t.Fatal("Log() error = nil, want a write error after the file was closed")
	}
	if got := logger.Seq(); got != 0 {
		t.Fatalf("Seq() after failed write = %d, want 0", got)
	}
}

func TestFilterAudioMatchesChannelImbalanceButIsSilenceDoesNot(t *testing.T) {
	t.Parallel()
	imbalance := []EventType{ChannelImbalanceStart, ChannelImbalanceEnd}
	for _, ty := range imbalance {
		if !IsChannelImbalanceEvent(ty) {
			t.Errorf("IsChannelImbalanceEvent(%s) = false, want true", ty)
		}
		if IsSilenceEvent(ty) {
			t.Errorf("IsSilenceEvent(%s) = true, want false (must not pollute the silence filter)", ty)
		}
		if !matchesFilter(ty, FilterAudio) {
			t.Errorf("matchesFilter(%s, FilterAudio) = false, want true", ty)
		}
	}
	silence := []EventType{SilenceStart, SilenceEnd, AudioDumpReady}
	for _, ty := range silence {
		if IsChannelImbalanceEvent(ty) {
			t.Errorf("IsChannelImbalanceEvent(%s) = true, want false", ty)
		}
		if !matchesFilter(ty, FilterAudio) {
			t.Errorf("matchesFilter(%s, FilterAudio) = false, want true", ty)
		}
	}
}
func TestReadLastIncludesChannelImbalanceUnderAudioFilter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	writeEvents(t, path, []Event{
		{Type: StreamStarted, Message: "stream"},
		{Type: ChannelImbalanceStart, Message: "imbalance-start"},
		{Type: ChannelImbalanceEnd, Message: "imbalance-end"},
	})
	got, _, err := ReadLast(path, 10, 0, FilterAudio)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	assertMessages(t, got, []string{"imbalance-end", "imbalance-start"})
}

func TestEventJSONDoesNotPersistClassification(t *testing.T) {
	t.Parallel()

	line := mustMarshal(t, &Event{Type: UploadFailed})
	for _, want := range []string{`"type":"upload_failed"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("marshaled event = %s, want to contain %s", line, want)
		}
	}
	for _, forbidden := range []string{`"severity"`, `"category"`, `"reason"`} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("marshaled event = %s, want no %s field", line, forbidden)
		}
	}
}

func TestLogStreamPersistsModeInDetails(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if err := logger.LogStream(
		StreamStarted,
		"stream-1",
		"Main Stream",
		"listener",
		"Listening on 0.0.0.0:9000",
		"",
		0,
		0,
	); err != nil {
		t.Fatalf("LogStream() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	//nolint:gosec // Test reads a controlled temp path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var event struct {
		Details StreamDetails `json:"details"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got := event.Details.Mode; got != "listener" {
		t.Fatalf("details.mode = %q, want listener", got)
	}
}

func writeEvents(t testing.TB, path string, events []Event) {
	t.Helper()
	//nolint:gosec // Test path is under t.TempDir.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
