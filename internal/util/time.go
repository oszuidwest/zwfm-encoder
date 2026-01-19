package util

import (
	"fmt"
	"regexp"
	"time"
)

// TimestampPatterns defines regex patterns and layouts for parsing timestamps
// from filenames, ordered from most specific to least specific.
var TimestampPatterns = []struct {
	Pattern *regexp.Regexp
	Layout  string
}{
	// YYYY-MM-DD_HH-MM-SS (silencedump format)
	{regexp.MustCompile(`(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})`), "2006-01-02_15-04-05"},
	// YYYY-MM-DD-HH-MM (recording format)
	{regexp.MustCompile(`(\d{4}-\d{2}-\d{2}-\d{2}-\d{2})`), "2006-01-02-15-04"},
	// YYYY-MM-DD (date only fallback)
	{regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`), "2006-01-02"},
}

// OldestToKeep returns the oldest timestamp to retain during cleanup.
// Files older than this should be deleted.
//
// Uses rolling retention: now - (days * 24 hours).
func OldestToKeep(days int, now time.Time) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// TimeUntilNextHour calculates the duration until the next clock hour boundary.
func TimeUntilNextHour(t time.Time) time.Duration {
	nextHour := t.Truncate(time.Hour).Add(time.Hour)
	return nextHour.Sub(t)
}

// FilenameTime parses a timestamp from a filename string.
// Supports: YYYY-MM-DD_HH-MM-SS, YYYY-MM-DD-HH-MM, YYYY-MM-DD.
func FilenameTime(filename string) (time.Time, bool) {
	for _, p := range TimestampPatterns {
		if m := p.Pattern.FindStringSubmatch(filename); len(m) >= 2 {
			if ts, err := time.ParseInLocation(p.Layout, m[1], time.Local); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// humanTimeFormat is the layout for human-readable timestamps with timezone.
const humanTimeFormat = "2 Jan 2006 15:04:05 MST"

// HumanTime returns the current local time formatted for display in notifications.
func HumanTime() string {
	return time.Now().Format(humanTimeFormat)
}

// FormatHumanTime converts an RFC3339 timestamp to a display-friendly local time format.
func FormatHumanTime(rfc3339 string) string {
	if rfc3339 == "" || rfc3339 == "unknown" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format(humanTimeFormat)
}

// FormatDuration converts milliseconds to a human-readable duration string.
// Shows only the two most relevant units:
//   - < 1 minute: "45s"
//   - < 1 hour: "12m 34s"
//   - < 24 hours: "2h 24m"
//   - >= 24 hours: "1d 3h"
func FormatDuration(ms int64) string {
	totalSeconds := ms / 1000
	if totalSeconds < 60 {
		return fmt.Sprintf("%ds", totalSeconds)
	}

	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}

	hours := minutes / 60
	minutes %= 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}

	days := hours / 24
	hours %= 24
	return fmt.Sprintf("%dd %dh", days, hours)
}
