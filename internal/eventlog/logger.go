// Package eventlog provides unified event logging for the encoder.
// It stores stream, audio, and recorder events in one JSON lines file.
package eventlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// EventType identifies the category of a logged event.
type EventType string

const (
	// StreamStarted indicates a stream start event.
	StreamStarted EventType = "stream_started"
	// StreamStable indicates a stream stable event.
	StreamStable EventType = "stream_stable"
	// StreamError indicates a stream error event.
	StreamError EventType = "stream_error"
	// StreamRetry indicates a stream retry event.
	StreamRetry EventType = "stream_retry"
	// StreamStopped indicates a stream stopped event.
	StreamStopped EventType = "stream_stopped"
)

const (
	// SilenceStart indicates a silence start event.
	SilenceStart EventType = "silence_start"
	// SilenceEnd indicates a silence end event.
	SilenceEnd EventType = "silence_end"
	// AudioDumpReady indicates an audio dump MP3 is ready after a silence event.
	AudioDumpReady EventType = "audio_dump_ready"
	// ChannelImbalanceStart indicates an L/R channel imbalance start event.
	ChannelImbalanceStart EventType = "channel_imbalance_start"
	// ChannelImbalanceEnd indicates an L/R channel imbalance end event.
	ChannelImbalanceEnd EventType = "channel_imbalance_end"
)

const (
	// RecorderStarted indicates a recorder started event.
	RecorderStarted EventType = "recorder_started"
	// RecorderStopped indicates a recorder stopped event.
	RecorderStopped EventType = "recorder_stopped"
	// RecorderError indicates a recorder error event.
	RecorderError EventType = "recorder_error"
	// RecorderFile indicates a recorder file rotation event.
	RecorderFile EventType = "recorder_file"
	// UploadQueued indicates an upload queued event.
	UploadQueued EventType = "upload_queued"
	// UploadCompleted indicates an upload completed event.
	UploadCompleted EventType = "upload_completed"
	// UploadFailed indicates an upload failed event.
	UploadFailed EventType = "upload_failed"
	// UploadRetry indicates an upload retry event.
	UploadRetry EventType = "upload_retry"
	// UploadAbandoned indicates an upload abandoned event.
	UploadAbandoned EventType = "upload_abandoned"
	// CleanupCompleted indicates a cleanup completed event.
	CleanupCompleted EventType = "cleanup_completed"
)

// Event is a single log entry with type-specific details.
type Event struct {
	Timestamp time.Time `json:"ts"` // RFC3339
	Type      EventType `json:"type"`
	StreamID  string    `json:"stream_id,omitempty"`
	Message   string    `json:"msg,omitempty"`
	Details   any       `json:"details,omitempty"`
}

// StreamDetails holds stream event information.
type StreamDetails struct {
	StreamName string `json:"stream_name,omitempty"`
	Mode       string `json:"mode"`
	Error      string `json:"error,omitempty"`
	RetryCount int    `json:"retry,omitempty"`
	MaxRetries int    `json:"max_retries,omitempty"`
}

// SilenceDetails holds silence event information.
type SilenceDetails struct {
	LevelLeftDB   float64 `json:"level_left_db"`  // dB
	LevelRightDB  float64 `json:"level_right_db"` // dB
	ThresholdDB   float64 `json:"threshold_db"`   // dB
	DurationMs    int64   `json:"duration_ms,omitempty"`
	DumpPath      string  `json:"dump_path,omitempty"`
	DumpFilename  string  `json:"dump_filename,omitempty"`
	DumpSizeBytes int64   `json:"dump_size_bytes,omitempty"`
	DumpError     string  `json:"dump_error,omitempty"`
}

// ImbalanceDetails holds channel imbalance event information.
type ImbalanceDetails struct {
	LevelLeftDB  float64 `json:"level_left_db"`  // dB
	LevelRightDB float64 `json:"level_right_db"` // dB
	BalanceDB    float64 `json:"balance_db"`     // dB; signed L-R
	ImbalanceDB  float64 `json:"imbalance_db"`   // dB; abs(L-R)
	ThresholdDB  float64 `json:"threshold_db"`   // dB
	DurationMs   int64   `json:"duration_ms,omitempty"`
}

// RecorderDetails holds recorder event information.
type RecorderDetails struct {
	RecorderName string `json:"recorder_name,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Codec        string `json:"codec,omitempty"`
	StorageMode  string `json:"storage_mode,omitempty"`
	S3Key        string `json:"s3_key,omitempty"`
	Error        string `json:"error,omitempty"`
	RetryCount   int    `json:"retry,omitempty"`
	FilesDeleted int    `json:"files_deleted,omitempty"`
	StorageType  string `json:"storage_type,omitempty"`
}

// Logger records events to a JSON lines file. Rotation is handled by a
// util.RollingWriter and is best-effort: a rotation blocked by another
// process (antivirus, indexer) never fails a Log call; only genuine write
// failures surface. Each event is marshaled up front and issued as a single
// Write, so a record is never split across the active and rotated file, and
// a failed write never poisons the logger: the writer self-heals and the
// next event is attempted fresh. (A long-lived json.Encoder would cache the
// first write error forever and defeat that recovery.)
type Logger struct {
	mu       sync.Mutex // serializes Log: timestamp fill, marshal, write, seq
	filePath string
	writer   *util.RollingWriter
	seq      atomic.Uint64 // incremented on each written event; a change signal for live views
}

const (
	// DefaultMaxLogSizeBytes is the active JSONL log size that triggers rotation.
	DefaultMaxLogSizeBytes int64 = 50 * 1024 * 1024
	tailReadChunkSize      int64 = 64 * 1024
)

// DefaultLogPath returns the platform-specific log file path.
func DefaultLogPath(port int) string {
	portStr := strconv.Itoa(port)
	switch runtime.GOOS {
	case "darwin":
		configDir, err := os.UserConfigDir()
		if err != nil {
			configDir = os.TempDir()
		}
		return filepath.Join(configDir, "encoder", "logs", portStr, "encoder.jsonl")
	case "windows":
		// %PROGRAMDATA% is typically C:\ProgramData
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "encoder", "logs", portStr, "encoder.jsonl")
	default:
		//nolint:gocritic // Intentional absolute path for Unix systems
		return filepath.Join("/var/log/encoder", portStr, "encoder.jsonl")
	}
}

// NewLogger creates a new event logger at the specified path.
func NewLogger(filePath string) (*Logger, error) {
	return newLogger(filePath, DefaultMaxLogSizeBytes)
}

func newLogger(filePath string, maxSizeBytes int64) (*Logger, error) {
	writer, err := util.NewRollingWriter(filePath, maxSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	return &Logger{
		filePath: filePath,
		writer:   writer,
	}, nil
}

// Log writes an event, using the current time if Timestamp is zero.
// Concurrent calls are safe, even with the same *Event: the logger mutex
// covers the timestamp fill and marshal, not just the file write.
func (l *Logger) Log(event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := l.writer.Write(append(data, '\n')); err != nil {
		return err
	}
	l.seq.Add(1)
	return nil
}

// LogStream records a stream event with error and retry details.
func (l *Logger) LogStream(
	eventType EventType, streamID, streamName, mode, message, errMsg string,
	retryCount, maxRetries int,
) error {
	return l.Log(&Event{
		Type:     eventType,
		StreamID: streamID,
		Message:  message,
		Details: &StreamDetails{
			StreamName: streamName,
			Mode:       mode,
			Error:      errMsg,
			RetryCount: retryCount,
			MaxRetries: maxRetries,
		},
	})
}

// LogSilenceStart records when silence is first detected.
// t must be captured at the moment the event occurs so the timestamp is
// accurate even when the write is deferred to a background goroutine.
func (l *Logger) LogSilenceStart(t time.Time, levelL, levelR, threshold float64) error {
	return l.Log(&Event{
		Timestamp: t,
		Type:      SilenceStart,
		Details: &SilenceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			ThresholdDB:  threshold,
		},
	})
}

// LogSilenceEnd records when silence ends with duration information.
// t must be captured at the moment the event occurs so the timestamp is
// accurate even when the write is deferred to a background goroutine.
func (l *Logger) LogSilenceEnd(t time.Time, durationMs int64, levelL, levelR, threshold float64) error {
	return l.Log(&Event{
		Timestamp: t,
		Type:      SilenceEnd,
		Details: &SilenceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			ThresholdDB:  threshold,
			DurationMs:   durationMs,
		},
	})
}

// LogAudioDumpReady records when the audio dump MP3 is ready after a silence event.
// t must be captured at the moment the event occurs so the timestamp is
// accurate even when the write is deferred to a background goroutine.
func (l *Logger) LogAudioDumpReady(
	t time.Time, durationMs int64, levelL, levelR, threshold float64,
	dumpPath, dumpFilename string, dumpSize int64, dumpError string,
) error {
	return l.Log(&Event{
		Timestamp: t,
		Type:      AudioDumpReady,
		Details: &SilenceDetails{
			LevelLeftDB:   levelL,
			LevelRightDB:  levelR,
			ThresholdDB:   threshold,
			DurationMs:    durationMs,
			DumpPath:      dumpPath,
			DumpFilename:  dumpFilename,
			DumpSizeBytes: dumpSize,
			DumpError:     dumpError,
		},
	})
}

// LogChannelImbalanceStart records when an L/R imbalance is confirmed.
// t must be the event time, even if the write is deferred.
func (l *Logger) LogChannelImbalanceStart(t time.Time, levelL, levelR, balanceDB, imbalanceDB, threshold float64) error {
	return l.Log(&Event{
		Timestamp: t,
		Type:      ChannelImbalanceStart,
		Details: &ImbalanceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			BalanceDB:    balanceDB,
			ImbalanceDB:  imbalanceDB,
			ThresholdDB:  threshold,
		},
	})
}

// LogChannelImbalanceEnd records when an L/R imbalance clears.
// t must be the event time, even if the write is deferred.
func (l *Logger) LogChannelImbalanceEnd(t time.Time, durationMs int64, levelL, levelR, balanceDB, imbalanceDB, threshold float64) error {
	return l.Log(&Event{
		Timestamp: t,
		Type:      ChannelImbalanceEnd,
		Details: &ImbalanceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			BalanceDB:    balanceDB,
			ImbalanceDB:  imbalanceDB,
			ThresholdDB:  threshold,
			DurationMs:   durationMs,
		},
	})
}

// LogRecorder records a recorder lifecycle or upload event. The details are
// stored on the event as-is; callers must not mutate them afterwards.
func (l *Logger) LogRecorder(eventType EventType, d *RecorderDetails) error {
	return l.Log(&Event{
		Type:    eventType,
		Details: d,
	})
}

// Close releases the log file handle. Close is terminal: subsequent Log
// calls fail with [os.ErrClosed].
func (l *Logger) Close() error {
	return l.writer.Close()
}

// Path returns the log file location.
func (l *Logger) Path() string {
	return l.filePath
}

// Seq returns a counter that increments on every written event. It is a
// lightweight change signal so live views can refetch only when a new event has
// been logged. The counter is in-memory and resets to zero on restart, so
// consumers must treat it as an opaque token and compare successive values
// rather than rely on its absolute magnitude.
func (l *Logger) Seq() uint64 {
	return l.seq.Load()
}

// TypeFilter selects event categories for [ReadLast].
type TypeFilter string

const (
	// FilterAll selects all events.
	FilterAll TypeFilter = ""
	// FilterStream selects stream events.
	FilterStream TypeFilter = "stream"
	// FilterAudio selects silence, audio dump, and channel imbalance events.
	FilterAudio TypeFilter = "audio"
	// FilterRecorder selects recorder events.
	FilterRecorder TypeFilter = "recorder"
)

// MaxReadLimit caps the number of events returned by [ReadLast].
const MaxReadLimit = 500

// ReadLast returns up to n events starting from offset, filtered by type.
// Events are returned in reverse chronological order (newest first).
// The second return value reports whether more events are available.
func ReadLast(filePath string, n, offset int, filter TypeFilter) ([]Event, bool, error) {
	// Cap n to prevent excessive memory allocation (defense in depth)
	n = min(n, MaxReadLimit)
	if n <= 0 {
		return []Event{}, false, nil
	}
	if offset < 0 {
		offset = 0
	}

	// Collect n+1 events to determine hasMore without a second loop
	events := make([]Event, 0, n+1)
	skipped := 0

	for _, path := range []string{filePath, util.RotatedPath(filePath)} {
		err := readLinesReverse(path, func(line []byte) (bool, error) {
			if len(events) > n {
				return false, nil
			}

			if len(line) == 0 {
				return true, nil
			}

			// Parse events in reverse order (newest first), applying filter.
			// Malformed lines can be left behind by crashes or manual edits.
			event, ok := parseEventLine(line)
			if !ok {
				return true, nil
			}

			if !matchesFilter(event.Type, filter) {
				return true, nil
			}

			// Skip events until we reach the offset.
			if skipped < offset {
				skipped++
				return true, nil
			}

			events = append(events, event)
			return len(events) <= n, nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, false, err
		}
		if len(events) > n {
			break
		}
	}

	// If we collected more than n, there are more events available
	hasMore := len(events) > n
	if hasMore {
		events = events[:n]
	}

	return events, hasMore, nil
}

func readLinesReverse(filePath string, handle func([]byte) (bool, error)) error {
	file, err := os.Open(filePath) //nolint:gosec // Event log paths come from trusted application state, not request input.
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // Read-only operation, close error not critical

	info, err := file.Stat()
	if err != nil {
		return err
	}

	offset := info.Size()
	var tail []byte
	for offset > 0 {
		size := min(tailReadChunkSize, offset)
		offset -= size

		chunk := make([]byte, int(size), int(size)+len(tail))
		if _, err := file.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return err
		}

		chunk = append(chunk, tail...)
		parts := bytes.Split(chunk, []byte{'\n'})
		for i := len(parts) - 1; i >= 1; i-- {
			cont, err := handle(parts[i])
			if err != nil {
				return err
			}
			if !cont {
				return nil
			}
		}
		tail = append([]byte(nil), parts[0]...)
	}

	if len(tail) == 0 {
		return nil
	}

	_, err = handle(tail)
	return err
}

func parseEventLine(line []byte) (Event, bool) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return Event{}, false
	}
	return event, true
}

// matchesFilter reports whether t matches the given filter. Filters map
// one-to-one onto the categories in eventClassifications, so a new event type
// only needs a classification entry to be filterable.
func matchesFilter(t EventType, filter TypeFilter) bool {
	switch filter {
	case FilterStream:
		return t.Category() == CategoryStream
	case FilterAudio:
		return t.Category() == CategoryAudio
	case FilterRecorder:
		return t.Category() == CategoryRecorder
	default: // FilterAll and unknown filters match everything
		return true
	}
}
