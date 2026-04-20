package notify

import (
	"log/slog"
	"time"
)

// timestampUTC returns the current UTC time in RFC3339 format.
func timestampUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// logNotifyResult logs the result of a notification attempt.
func logNotifyResult(fn func() error, channel, kind string) {
	err := fn()
	if err != nil {
		slog.Error("notification failed", "channel", channel, "kind", kind, "error", err)
	} else {
		slog.Info("notification sent", "channel", channel, "kind", kind)
	}
}
