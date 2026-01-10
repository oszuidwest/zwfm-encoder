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
	FilterAll     TypeFilter = ""
	FilterStream  TypeFilter = "stream"
	FilterSilence TypeFilter = "silence"
)

// ReadLast reads the last n events from the log file, optionally filtered by type.
func ReadLast(filePath string, n int, filter TypeFilter) ([]Event, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Read-only operation, close error not critical

	// Read all lines
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Parse events in reverse order (newest first), applying filter
	events := make([]Event, 0, n)
	for i := len(lines) - 1; i >= 0 && len(events) < n; i-- {
		var event Event
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			continue // Skip malformed lines
		}

		// Apply filter
		if filter != FilterAll {
			isStream := IsStreamEvent(event.Type)
			isSilence := IsSilenceEvent(event.Type)

			if filter == FilterStream && !isStream {
				continue
			}
			if filter == FilterSilence && !isSilence {
				continue
			}
		}

		events = append(events, event)
	}

	return events, nil
}

// IsStreamEvent returns true if the event type is a stream event.
func IsStreamEvent(t EventType) bool {
	return t == StreamStarted || t == StreamStable || t == StreamError || t == StreamRetry || t == StreamStopped
}

// IsSilenceEvent returns true if the event type is a silence event.
func IsSilenceEvent(t EventType) bool {
	return t == SilenceStart || t == SilenceEnd
}
