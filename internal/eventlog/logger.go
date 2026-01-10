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
	"sync"
	"time"
)

// EventType represents the type of event.
type EventType string

// Stream event types.
const (
	StreamStarted EventType = "stream_started"
	StreamStable  EventType = "stream_stable"
	StreamError   EventType = "stream_error"
	StreamRetry   EventType = "stream_retry"
	StreamStopped EventType = "stream_stopped"
)

// Silence event types.
const (
	SilenceStart EventType = "silence_start"
	SilenceEnd   EventType = "silence_end"
)

// Recorder event types.
const (
	RecorderStarted   EventType = "recorder_started"
	RecorderStopped   EventType = "recorder_stopped"
	RecorderError     EventType = "recorder_error"
	RecorderFile      EventType = "recorder_file"
	UploadQueued      EventType = "upload_queued"
	UploadCompleted   EventType = "upload_completed"
	UploadFailed      EventType = "upload_failed"
	CleanupCompleted  EventType = "cleanup_completed"
)

// Event represents a single log entry with type-specific details.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Type      EventType `json:"type"`
	StreamID  string    `json:"stream_id,omitempty"`
	Message   string    `json:"msg,omitempty"`
	Details   any       `json:"details,omitempty"`
}

// StreamDetails contains stream-specific event details.
type StreamDetails struct {
	StreamName string `json:"stream_name,omitempty"`
	Error      string `json:"error,omitempty"`
	RetryCount int    `json:"retry,omitempty"`
	MaxRetries int    `json:"max_retries,omitempty"`
}

// SilenceDetails contains silence-specific event details.
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

// RecorderDetails contains recorder-specific event details.
type RecorderDetails struct {
	RecorderName  string `json:"recorder_name,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Codec         string `json:"codec,omitempty"`
	StorageMode   string `json:"storage_mode,omitempty"`
	S3Key         string `json:"s3_key,omitempty"`
	Error         string `json:"error,omitempty"`
	RetryCount    int    `json:"retry,omitempty"`
	FilesDeleted  int    `json:"files_deleted,omitempty"`
	StorageType   string `json:"storage_type,omitempty"` // "local" or "s3" for cleanup
}

// Logger writes events to a JSON lines file.
type Logger struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	encoder  *json.Encoder
}

// DefaultLogPath returns the platform-specific log file path.
func DefaultLogPath(port int) string {
	switch runtime.GOOS {
	case "windows":
		// %PROGRAMDATA% is typically C:\ProgramData
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "encoder", "logs", fmt.Sprintf("%d", port), "encoder.jsonl")
	default: // linux, darwin
		//nolint:gocritic // Intentional absolute path for Unix systems
		return filepath.Join("/var/log/encoder", fmt.Sprintf("%d", port), "encoder.jsonl")
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

// Log writes an event to the log file.
func (l *Logger) Log(event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	return l.encoder.Encode(event)
}

// LogStream logs a stream event.
func (l *Logger) LogStream(eventType EventType, streamID, streamName, message, errMsg string, retryCount, maxRetries int) error {
	return l.Log(&Event{
		Timestamp: time.Now(),
		Type:      eventType,
		StreamID:  streamID,
		Message:   message,
		Details: &StreamDetails{
			StreamName: streamName,
			Error:      errMsg,
			RetryCount: retryCount,
			MaxRetries: maxRetries,
		},
	})
}

// LogSilenceStart logs a silence start event.
func (l *Logger) LogSilenceStart(levelL, levelR, threshold float64) error {
	return l.Log(&Event{
		Timestamp: time.Now(),
		Type:      SilenceStart,
		Details: &SilenceDetails{
			LevelLeftDB:  levelL,
			LevelRightDB: levelR,
			ThresholdDB:  threshold,
		},
	})
}

// LogSilenceEnd logs a silence end event with optional dump info.
func (l *Logger) LogSilenceEnd(durationMs int64, levelL, levelR, threshold float64, dumpPath, dumpFilename string, dumpSize int64, dumpError string) error {
	return l.Log(&Event{
		Timestamp: time.Now(),
		Type:      SilenceEnd,
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

// LogRecorder logs a recorder event.
func (l *Logger) LogRecorder(eventType EventType, recorderName, filename, codec, storageMode, s3Key, errMsg string, retryCount, filesDeleted int, storageType string) error {
	return l.Log(&Event{
		Timestamp: time.Now(),
		Type:      eventType,
		Message:   "",
		Details: &RecorderDetails{
			RecorderName: recorderName,
			Filename:     filename,
			Codec:        codec,
			StorageMode:  storageMode,
			S3Key:        s3Key,
			Error:        errMsg,
			RetryCount:   retryCount,
			FilesDeleted: filesDeleted,
			StorageType:  storageType,
		},
	})
}

// Close closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Path returns the path to the log file.
func (l *Logger) Path() string {
	return l.filePath
}

// TypeFilter specifies which event types to include when reading.
type TypeFilter string

// Filter constants for ReadLast.
const (
	FilterAll      TypeFilter = ""
	FilterStream   TypeFilter = "stream"
	FilterSilence  TypeFilter = "silence"
	FilterRecorder TypeFilter = "recorder"
)

// MaxReadLimit is the maximum number of events that can be read at once.
// This prevents denial-of-service via excessive memory allocation.
const MaxReadLimit = 500

// ReadLast reads events from the log file with pagination support.
// Returns up to n events starting from offset, filtered by type.
// Events are returned in reverse chronological order (newest first).
// The n parameter is capped at MaxReadLimit to prevent excessive memory allocation.
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
	events := make([]Event, 0, n)
	skipped := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var event Event
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			continue // Skip malformed lines
		}

		// Apply filter
		if filter != FilterAll {
			isStream := IsStreamEvent(event.Type)
			isSilence := IsSilenceEvent(event.Type)
			isRecorder := IsRecorderEvent(event.Type)

			if filter == FilterStream && !isStream {
				continue
			}
			if filter == FilterSilence && !isSilence {
				continue
			}
			if filter == FilterRecorder && !isRecorder {
				continue
			}
		}

		// Skip events until we reach the offset
		if skipped < offset {
			skipped++
			continue
		}

		events = append(events, event)

		// Stop if we have enough events
		if len(events) >= n {
			break
		}
	}

	// Check if there are more events available
	hasMore := false
	if len(events) == n {
		// Continue scanning to see if there's at least one more event
		for i := len(lines) - 1 - offset - n; i >= 0; i-- {
			var event Event
			if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
				continue
			}
			if filter != FilterAll {
				if filter == FilterStream && !IsStreamEvent(event.Type) {
					continue
				}
				if filter == FilterSilence && !IsSilenceEvent(event.Type) {
					continue
				}
				if filter == FilterRecorder && !IsRecorderEvent(event.Type) {
					continue
				}
			}
			hasMore = true
			break
		}
	}

	return events, hasMore, nil
}

// IsStreamEvent returns true if the event type is a stream event.
func IsStreamEvent(t EventType) bool {
	return t == StreamStarted || t == StreamStable || t == StreamError || t == StreamRetry || t == StreamStopped
}

// IsSilenceEvent returns true if the event type is a silence event.
func IsSilenceEvent(t EventType) bool {
	return t == SilenceStart || t == SilenceEnd
}

// IsRecorderEvent returns true if the event type is a recorder event.
func IsRecorderEvent(t EventType) bool {
	return t == RecorderStarted || t == RecorderStopped || t == RecorderError ||
		t == RecorderFile || t == UploadQueued || t == UploadCompleted ||
		t == UploadFailed || t == CleanupCompleted
}
