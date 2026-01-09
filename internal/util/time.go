package util

import (
	"fmt"
	"regexp"
	"time"
)

// DatePattern matches YYYY-MM-DD in filenames.
var DatePattern = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)

// RetentionCutoff returns the cutoff time for retention-based cleanup.
// Files with dates before this cutoff should be deleted.
//
// The cutoff is calculated as midnight (00:00:00) such that exactly N days
// of files are kept (today plus N-1 previous days).
//
// Example with retention=1, cleanup on Jan 10:
//
//	Cutoff = Jan 10 00:00:00
//	File from Jan 10 → kept (today)
//	File from Jan 9  → deleted (older than 1 day)
//
// Example with retention=7, cleanup on Jan 10:
//
//	Cutoff = Jan 4 00:00:00
//	Files from Jan 4-10 → kept (7 days)
//	Files from Jan 3 or earlier → deleted
func RetentionCutoff(days int) time.Time {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return today.AddDate(0, 0, -days+1)
}

// TimeUntilNextHour returns the duration until the next hour boundary.
func TimeUntilNextHour(t time.Time) time.Duration {
	nextHour := t.Truncate(time.Hour).Add(time.Hour)
	return nextHour.Sub(t)
}

// ExtractDateFromFilename extracts a date from a filename containing YYYY-MM-DD.
func ExtractDateFromFilename(filename string) (time.Time, bool) {
	matches := DatePattern.FindStringSubmatch(filename)
	if len(matches) < 2 {
		return time.Time{}, false
	}

	date, err := time.Parse(time.DateOnly, matches[1])
	if err != nil {
		return time.Time{}, false
	}

	return date, true
}

// humanTimeFormat is the layout for human-readable timestamps with timezone.
const humanTimeFormat = "2 Jan 2006 15:04:05 MST"

// HumanTime returns the current local time in a human-readable format.
func HumanTime() string {
	return time.Now().Format(humanTimeFormat)
}

// FormatHumanTime converts an RFC3339 timestamp to human-readable local time format.
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

// FormatDuration formats milliseconds as a human-readable duration string.
// Examples: "45s", "2m 34s", "1h 23m"
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
	return fmt.Sprintf("%dh %dm", hours, minutes)
}
