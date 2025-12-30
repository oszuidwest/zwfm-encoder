package notify

import "time"

// timestampUTC returns the current UTC time in RFC3339 format.
func timestampUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
