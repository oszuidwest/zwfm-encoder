package util

import "log/slog"

// LogNotifyResult logs the result of a notification attempt.
func LogNotifyResult(fn func() error, notifyType string) {
	err := fn()
	if err != nil {
		slog.Error("notification failed", "type", notifyType, "error", err)
	} else {
		slog.Info("notification sent", "type", notifyType)
	}
}
