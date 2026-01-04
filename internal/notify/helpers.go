package notify

import "log/slog"

// logNotifyResult logs the result of a notification attempt.
func logNotifyResult(fn func() error, notifyType string) {
	err := fn()
	if err != nil {
		slog.Error("notification failed", "type", notifyType, "error", err)
	} else {
		slog.Info("notification sent", "type", notifyType)
	}
}
