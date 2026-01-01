package notify

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// LogSilenceStart records the beginning of a silence event.
func LogSilenceStart(logPath string, threshold float64) error {
	return appendLogEntry(logPath, types.SilenceLogEntry{
		Timestamp:   timestampUTC(),
		Event:       "silence_start",
		ThresholdDB: threshold,
	})
}

// LogSilenceEnd records the end of a silence event with its total duration.
func LogSilenceEnd(logPath string, silenceDurationMs int64, threshold float64) error {
	return appendLogEntry(logPath, types.SilenceLogEntry{
		Timestamp:   timestampUTC(),
		Event:       "silence_end",
		DurationMs:  silenceDurationMs,
		ThresholdDB: threshold,
	})
}

// WriteTestLog writes a test entry to verify log file configuration.
func WriteTestLog(logPath string) error {
	if logPath == "" {
		return fmt.Errorf("log file path not configured")
	}

	return appendLogEntry(logPath, types.SilenceLogEntry{
		Timestamp:   timestampUTC(),
		Event:       "test",
		DurationMs:  0,
		ThresholdDB: 0,
	})
}

// appendLogEntry appends a JSON log entry to the file.
func appendLogEntry(logPath string, entry types.SilenceLogEntry) error {
	if !util.IsConfigured(logPath) {
		return nil
	}

	jsonData, err := json.Marshal(entry)
	if err != nil {
		return util.WrapError("marshal log entry", err)
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return util.WrapError("open log file", err)
	}
	defer util.SafeCloseFunc(f, "log file")()

	if _, err := f.Write(jsonData); err != nil {
		return util.WrapError("write log entry", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return util.WrapError("write newline", err)
	}

	return nil
}
