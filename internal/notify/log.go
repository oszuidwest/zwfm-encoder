package notify

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// LogSilenceStart records the beginning of a silence event.
func LogSilenceStart(logPath string, levelL, levelR, threshold float64) error {
	return appendLogEntry(logPath, &types.SilenceLogEntry{
		Timestamp:    timestampUTC(),
		Event:        "silence_start",
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		ThresholdDB:  threshold,
	})
}

// LogSilenceEndWithDump records the end of a silence event with optional dump info.
func LogSilenceEndWithDump(logPath string, silenceDurationMs int64, levelL, levelR, threshold float64, dump *silencedump.EncodeResult) error {
	entry := &types.SilenceLogEntry{
		Timestamp:    timestampUTC(),
		Event:        "silence_end",
		DurationMs:   silenceDurationMs,
		LevelLeftDB:  levelL,
		LevelRightDB: levelR,
		ThresholdDB:  threshold,
	}

	if dump != nil {
		if dump.Error != nil {
			entry.DumpError = dump.Error.Error()
		} else {
			entry.DumpPath = dump.FilePath
			entry.DumpFilename = dump.Filename
			entry.DumpSizeBytes = dump.FileSize
		}
	}

	return appendLogEntry(logPath, entry)
}

// WriteTestLog writes a test log entry.
func WriteTestLog(logPath string) error {
	if logPath == "" {
		return fmt.Errorf("log file path not configured")
	}

	return appendLogEntry(logPath, &types.SilenceLogEntry{
		Timestamp:   timestampUTC(),
		Event:       "test",
		DurationMs:  0,
		ThresholdDB: 0,
	})
}

// appendLogEntry appends a log entry to the file.
func appendLogEntry(logPath string, entry *types.SilenceLogEntry) error {
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
