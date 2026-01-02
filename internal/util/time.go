package util

import "time"

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
