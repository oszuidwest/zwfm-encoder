package notify

import "time"

// AppName is the application name used in notifications.
const AppName = "ZuidWest FM Encoder"

// timestampUTC returns the current UTC time in RFC3339 format.
func timestampUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
