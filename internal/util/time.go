package util

import (
	"fmt"
	"time"
)

// humanTimeFormat is the layout for human-readable timestamps with timezone.
const humanTimeFormat = "2 Jan 2006 15:04 MST"

// HumanTime returns the current local time in a human-readable format.
func HumanTime() string {
	return time.Now().Format(humanTimeFormat)
}

// FormatHumanTime parses an RFC3339 string and returns human-readable format in local time.
// Returns "unknown" for empty input, or the original string if parsing fails.
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
