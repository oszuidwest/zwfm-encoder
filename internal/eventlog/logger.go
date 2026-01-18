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
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"ts"`
	// Type identifies the event category.
	Type EventType `json:"type"`
	// StreamID is the associated stream ID, when applicable.
	StreamID string `json:"stream_id,omitempty"`
	// Message provides a human-readable summary.
	Message string `json:"msg,omitempty"`
	// Details holds type-specific event data.
	Details any `json:"details,omitempty"`
}

// StreamDetails holds stream event information.
type StreamDetails struct {
	// StreamName is the display name of the stream.
	StreamName string `json:"stream_name,omitempty"`
	// Error contains the stream error message, if any.
	Error string `json:"error,omitempty"`
	// RetryCount is the current retry attempt.
	RetryCount int `json:"retry,omitempty"`
	// MaxRetries is the maximum allowed retry attempts.
	MaxRetries int `json:"max_retries,omitempty"`
}

// SilenceDetails holds silence event information.
type SilenceDetails struct {
	// LevelLeftDB is the left channel level in dB at detection time.
	LevelLeftDB float64 `json:"level_left_db"`
	// LevelRightDB is the right channel level in dB at detection time.
	LevelRightDB float64 `json:"level_right_db"`
	// ThresholdDB is the silence threshold in dB used for detection.
	ThresholdDB float64 `json:"threshold_db"`
	// DurationMs is the silence duration in milliseconds.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// DumpPath is the full path to the audio dump file.
	DumpPath string `json:"dump_path,omitempty"`
	// DumpFilename is the audio dump file name.
	DumpFilename string `json:"dump_filename,omitempty"`
	// DumpSizeBytes is the audio dump size in bytes.
	DumpSizeBytes int64 `json:"dump_size_bytes,omitempty"`
	// DumpError is the error message from dump encoding, if any.
	DumpError string `json:"dump_error,omitempty"`
}

// RecorderDetails holds recorder event information.
type RecorderDetails struct {
	// RecorderName is the recorder display name.
	RecorderName string `json:"recorder_name,omitempty"`
	// Filename is the recording file name.
	Filename string `json:"filename,omitempty"`
	// Codec is the encoding codec used for the recording.
	Codec string `json:"codec,omitempty"`
	// StorageMode is the configured storage mode.
	StorageMode string `json:"storage_mode,omitempty"`
	// S3Key is the object key used for S3 uploads.
	S3Key string `json:"s3_key,omitempty"`
	// Error contains the recorder error message, if any.
	Error string `json:"error,omitempty"`
	// RetryCount is the upload retry attempt.
	RetryCount int `json:"retry,omitempty"`
	// FilesDeleted is the number of files deleted during cleanup.
	FilesDeleted int `json:"files_deleted,omitempty"`
	// StorageType identifies the cleanup target storage type (local or s3).
	StorageType string `json:"storage_type,omitempty"`
}

// RecorderEventParams provides optional fields for [Logger.LogRecorder].
type RecorderEventParams struct {
	// RecorderName is the recorder display name.
	RecorderName string
	// Filename is the recording file name.
	Filename string
	// Codec is the encoding codec used for the recording.
	Codec string
	// StorageMode is the storage mode.
	StorageMode string
	// S3Key is the object key used for S3 uploads.
	S3Key string
	// Error contains the recorder error message, if any.
	Error string
	// RetryCount is the upload retry attempt.
	RetryCount int
	// FilesDeleted is the number of files deleted during cleanup.
	FilesDeleted int
	// StorageType identifies the cleanup target storage type (local or s3).
	StorageType string
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

const (
	// FilterAll selects all events.
	FilterAll TypeFilter = ""
	// FilterStream selects stream events.
	FilterStream TypeFilter = "stream"
	// FilterAudio selects silence events.
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
