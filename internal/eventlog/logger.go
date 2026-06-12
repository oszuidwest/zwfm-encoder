// Package eventlog provides unified event logging for the encoder.
// It captures both stream events (started, stable, error, retry, stopped)
// and silence events (silence_start, silence_end, audio_dump_ready) in a single JSON lines file.
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
	// AudioDumpReady indicates an audio dump MP3 is ready after a silence event.
	AudioDumpReady EventType = "audio_dump_ready"
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
	mu           sync.Mutex
	filePath     string
	file         *os.File
	encoder      *json.Encoder
	maxSizeBytes int64
}

const (
	// DefaultMaxLogSizeBytes is the active JSONL log size that triggers rotation.
	DefaultMaxLogSizeBytes int64 = 50 * 1024 * 1024
	rotatedLogSuffix             = ".1"
	tailReadChunkSize      int64 = 64 * 1024
)

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
	return newLogger(filePath, DefaultMaxLogSizeBytes)
}

func newLogger(filePath string, maxSizeBytes int64) (*Logger, error) {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Log directory needs to be readable
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	// Open file for appending
	//nolint:gosec // Log file needs to be readable
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	return &Logger{
		filePath:     filePath,
		file:         file,
		encoder:      json.NewEncoder(file),
		maxSizeBytes: maxSizeBytes,
	}, nil
}

// Log writes an event, using the current time if Timestamp is zero.
func (l *Logger) Log(event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	if err := l.encoder.Encode(event); err != nil {
		return err
	}

	return l.rotateIfNeededLocked()
}

func (l *Logger) rotateIfNeededLocked() error {
	if l.maxSizeBytes <= 0 || l.file == nil {
		return nil
	}

	info, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("stat log file: %w", err)
	}
	if info.Size() == 0 || info.Size() < l.maxSizeBytes {
		return nil
	}

	return l.rotateLocked()
}

func (l *Logger) rotateLocked() error {
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	l.file = nil
	l.encoder = nil

	rotatedPath := rotatedLogPath(l.filePath)
	if err := os.Remove(rotatedPath); err != nil && !os.IsNotExist(err) {
		if reopenErr := l.openLocked(); reopenErr != nil {
			return fmt.Errorf("remove rotated log: %w; reopen log: %w", err, reopenErr)
		}
		return fmt.Errorf("remove rotated log: %w", err)
	}

	if err := os.Rename(l.filePath, rotatedPath); err != nil {
		if reopenErr := l.openLocked(); reopenErr != nil {
			return fmt.Errorf("rotate log: %w; reopen log: %w", err, reopenErr)
		}
		return fmt.Errorf("rotate log: %w", err)
	}

	return l.openLocked()
}

func (l *Logger) openLocked() error {
	//nolint:gosec // Log file needs to be readable
	file, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	l.file = file
	l.encoder = json.NewEncoder(file)
	return nil
}

// LogStream records a stream event with error and retry details.
func (l *Logger) LogStream(
	eventType EventType, streamID, streamName, message, errMsg string,
	retryCount, maxRetries int,
) error {
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
	// FilterAudio selects silence and audio dump events (silence_start, silence_end, audio_dump_ready).
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
	if offset < 0 {
		offset = 0
	}

	// Collect n+1 events to determine hasMore without a second loop
	events := make([]Event, 0, n+1)
	skipped := 0

	for _, path := range []string{filePath, rotatedLogPath(filePath)} {
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
	file, err := os.Open(filePath) //nolint:gosec // filePath is from DefaultLogPath, not user input
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

		chunk := make([]byte, size)
		if _, err := file.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return err
		}

		data := make([]byte, len(chunk), len(chunk)+len(tail))
		copy(data, chunk)
		data = append(data, tail...)
		parts := bytes.Split(data, []byte{'\n'})
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

func rotatedLogPath(filePath string) string {
	return filePath + rotatedLogSuffix
}

func parseEventLine(line []byte) (Event, bool) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return Event{}, false
	}
	return event, true
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
	case SilenceStart, SilenceEnd, AudioDumpReady:
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
