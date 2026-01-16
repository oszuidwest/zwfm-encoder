// Package eventlog provides unified event logging for the encoder.
// It captures both stream events (started, stable, error, retry, stopped)
// and silence events (silence_start, silence_end) in a single JSON lines file.
package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// EventType identifies the category of a logged event.
type EventType string

// Stream event types: started, stable, error, retry, stopped.
const (
	StreamStarted EventType = "stream_started"
	StreamStable  EventType = "stream_stable"
	StreamError   EventType = "stream_error"
	StreamRetry   EventType = "stream_retry"
	StreamStopped EventType = "stream_stopped"
)

// Silence event types: start and end.
const (
	SilenceStart EventType = "silence_start"
	SilenceEnd   EventType = "silence_end"
)

// Recorder event types: lifecycle, upload, and cleanup events.
const (
	RecorderStarted  EventType = "recorder_started"
	RecorderStopped  EventType = "recorder_stopped"
	RecorderError    EventType = "recorder_error"
	RecorderFile     EventType = "recorder_file"
	UploadQueued     EventType = "upload_queued"
	UploadCompleted  EventType = "upload_completed"
	UploadFailed     EventType = "upload_failed"
	UploadRetry      EventType = "upload_retry"
	UploadAbandoned  EventType = "upload_abandoned"
	CleanupCompleted EventType = "cleanup_completed"
)

// Event is a single log entry with type-specific details.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Type      EventType `json:"type"`
	StreamID  string    `json:"stream_id,omitempty"`
	Message   string    `json:"msg,omitempty"`
	Details   any       `json:"details,omitempty"`
}

// StreamDetails holds stream event information.
type StreamDetails struct {
	StreamName string `json:"stream_name,omitempty"`
	Error      string `json:"error,omitempty"`
	RetryCount int    `json:"retry,omitempty"`
	MaxRetries int    `json:"max_retries,omitempty"`
}

// SilenceDetails holds silence event information.
type SilenceDetails struct {
	LevelLeftDB   float64 `json:"level_left_db"`
	LevelRightDB  float64 `json:"level_right_db"`
	ThresholdDB   float64 `json:"threshold_db"`
	DurationMs    int64   `json:"duration_ms,omitempty"`
	DumpPath      string  `json:"dump_path,omitempty"`
	DumpFilename  string  `json:"dump_filename,omitempty"`
	DumpSizeBytes int64   `json:"dump_size_bytes,omitempty"`
	DumpError     string  `json:"dump_error,omitempty"`
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
	StorageType  string `json:"storage_type,omitempty"` // "local" or "s3" for cleanup
}

// RecorderEventParams provides optional fields for [Logger.LogRecorder].
type RecorderEventParams struct {
	RecorderName string
	Filename     string
	Codec        string
	StorageMode  string
	S3Key        string
	Error        string
	RetryCount   int
	FilesDeleted int
	StorageType  string
}

// Logger records events to a JSON lines file.
type Logger struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	encoder  *json.Encoder
}

// DefaultLogPath returns the platform-specific log file path.
func DefaultLogPath(port int) string {
	portStr := strconv.Itoa(port)
	switch runtime.GOOS {
	case "windows":
		// %PROGRAMDATA% is typically C:\ProgramData
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "encoder", "logs", portStr, "encoder.jsonl")
	default: // linux, darwin
		//nolint:gocritic // Intentional absolute path for Unix systems
		return filepath.Join("/var/log/encoder", portStr, "encoder.jsonl")
	}
}

// NewLogger creates a new event logger at the specified path.
func NewLogger(filePath string) (*Logger, error) {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	// Open file for appending
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	return &Logger{
		filePath: filePath,
		file:     file,
		encoder:  json.NewEncoder(file),
	}, nil
}

// Log writes an event, using the current time if Timestamp is zero.
func (l *Logger) Log(event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	return l.encoder.Encode(event)
}

// LogStream records a stream event with error and retry details.
func (l *Logger) LogStream(eventType EventType, streamID, streamName, message, errMsg string, retryCount, maxRetries int) error {
	return l.Log(&Event{
		Type:     eventType,
		StreamID: streamID,
		Message:  message,
		Details: &StreamDetails{
			StreamName: streamName,
			Error:      errMsg,
			RetryCount: retryCount,
			MaxRetries: maxRetries,
		},
	})
}

// LogSilenceStart records when silence is first detected.
func (l *Logger) LogSilenceStart(levelL, levelR, threshold float64) error {
	return l.Log(&Event{
		Type: SilenceStart,
		Details: &SilenceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			ThresholdDB:  threshold,
		},
	})
}

// LogSilenceEnd records when silence ends, including duration and debug dump information.
func (l *Logger) LogSilenceEnd(durationMs int64, levelL, levelR, threshold float64, dumpPath, dumpFilename string, dumpSize int64, dumpError string) error {
	return l.Log(&Event{
		Type: SilenceEnd,
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

// LogRecorder records a recorder lifecycle or upload event.
func (l *Logger) LogRecorder(eventType EventType, p *RecorderEventParams) error {
	return l.Log(&Event{
		Type: eventType,
		Details: &RecorderDetails{
			RecorderName: p.RecorderName,
			Filename:     p.Filename,
			Codec:        p.Codec,
			StorageMode:  p.StorageMode,
			S3Key:        p.S3Key,
			Error:        p.Error,
			RetryCount:   p.RetryCount,
			FilesDeleted: p.FilesDeleted,
			StorageType:  p.StorageType,
		},
	})
}

// Close releases the log file handle.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Path returns the log file location.
func (l *Logger) Path() string {
	return l.filePath
}

// TypeFilter selects event categories for [ReadLast].
type TypeFilter string

// TypeFilter values for [ReadLast].
const (
	FilterAll      TypeFilter = ""
	FilterStream   TypeFilter = "stream"
	FilterAudio    TypeFilter = "audio"
	FilterRecorder TypeFilter = "recorder"
)

// MaxReadLimit caps the number of events returned by [ReadLast].
const MaxReadLimit = 500

// ReadLast returns up to n events starting from offset, filtered by type.
// Events are returned in reverse chronological order (newest first).
// The second return value reports whether more events are available.
func ReadLast(filePath string, n, offset int, filter TypeFilter) ([]Event, bool, error) {
	// Cap n to prevent excessive memory allocation (defense in depth)
	if n > MaxReadLimit {
		n = MaxReadLimit
	}
	if n <= 0 {
		return []Event{}, false, nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, false, nil
		}
		return nil, false, err
	}
	defer file.Close() //nolint:errcheck // Read-only operation, close error not critical

	// Read all lines
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}

	// Parse events in reverse order (newest first), applying filter
	// Collect n+1 events to determine hasMore without a second loop
	events := make([]Event, 0, n+1)
	skipped := 0
	for i := len(lines) - 1; i >= 0 && len(events) <= n; i-- {
		var event Event
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			continue // Skip malformed lines
		}

		if !matchesFilter(event.Type, filter) {
			continue
		}

		// Skip events until we reach the offset
		if skipped < offset {
			skipped++
			continue
		}

		events = append(events, event)
	}

	// If we collected more than n, there are more events available
	hasMore := len(events) > n
	if hasMore {
		events = events[:n]
	}

	return events, hasMore, nil
}

// matchesFilter reports whether t matches the given filter.
func matchesFilter(t EventType, filter TypeFilter) bool {
	switch filter {
	case FilterAll:
		return true
	case FilterStream:
		return IsStreamEvent(t)
	case FilterAudio:
		return IsSilenceEvent(t)
	case FilterRecorder:
		return IsRecorderEvent(t)
	default:
		return true
	}
}

// IsStreamEvent reports whether t is a stream event type.
func IsStreamEvent(t EventType) bool {
	switch t {
	case StreamStarted, StreamStable, StreamError, StreamRetry, StreamStopped:
		return true
	default:
		return false
	}
}

// IsSilenceEvent reports whether t is a silence event type.
func IsSilenceEvent(t EventType) bool {
	switch t {
	case SilenceStart, SilenceEnd:
		return true
	default:
		return false
	}
}

// IsRecorderEvent reports whether t is a recorder event type.
func IsRecorderEvent(t EventType) bool {
	switch t {
	case RecorderStarted, RecorderStopped, RecorderError, RecorderFile,
		UploadQueued, UploadCompleted, UploadFailed, UploadRetry, UploadAbandoned,
		CleanupCompleted:
		return true
	default:
		return false
	}
}
