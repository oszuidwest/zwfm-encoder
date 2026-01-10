package events

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventType represents the type of stream event.
type EventType string

const (
	EventStarted EventType = "started"
	EventStable  EventType = "stable"
	EventError   EventType = "error"
	EventRetry   EventType = "retry"
	EventStopped EventType = "stopped"
)

// StreamEvent represents a single stream event.
type StreamEvent struct {
	Timestamp  time.Time `json:"ts"`
	StreamID   string    `json:"stream_id"`
	StreamName string    `json:"stream_name"`
	Event      EventType `json:"event"`
	Message    string    `json:"msg"`
	Error      string    `json:"error,omitempty"`
	RetryCount int       `json:"retry,omitempty"`
	MaxRetries int       `json:"max_retries,omitempty"`
}

// Logger writes stream events to a JSON lines file.
type Logger struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	encoder  *json.Encoder
}

// NewLogger creates a new event logger.
func NewLogger(filePath string) (*Logger, error) {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Open file for appending
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	return &Logger{
		filePath: filePath,
		file:     file,
		encoder:  json.NewEncoder(file),
	}, nil
}

// Log writes an event to the log file.
func (l *Logger) Log(event *StreamEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	return l.encoder.Encode(event)
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

// ReadLast reads the last n events from the log file.
func ReadLast(filePath string, n int) ([]StreamEvent, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []StreamEvent{}, nil
		}
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Read-only operation, close error not critical

	// Read all lines (for simplicity; could optimize with reverse reading for large files)
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Take last n lines
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	lines = lines[start:]

	// Parse events (newest first)
	events := make([]StreamEvent, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var event StreamEvent
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			continue // Skip malformed lines
		}
		events = append(events, event)
	}

	return events, nil
}
