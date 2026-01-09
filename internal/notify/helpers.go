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
func logNotifyResult(fn func() error, notifyType string) {
	err := fn()
	if err != nil {
		slog.Error("notification failed", "type", notifyType, "error", err)
	} else {
		slog.Info("notification sent", "type", notifyType)
	}
}
