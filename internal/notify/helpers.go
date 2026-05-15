package notify

import (
	"errors"
	"log/slog"
	"time"
)

// logAttrsProvider is implemented by errors that carry extra structured
// attributes for slog logging.
type logAttrsProvider interface {
	LogAttrs() []any
}

// timestampUTC returns the current UTC time in RFC3339 format.
func timestampUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// logNotifyResult logs the result of a notification attempt.
func logNotifyResult(fn func() error, channel, kind string) {
	err := fn()
	if err != nil {
		attrs := []any{"channel", channel, "kind", kind, "error", err}
		var provider logAttrsProvider
		if errors.As(err, &provider) {
			attrs = append(attrs, provider.LogAttrs()...)
		}
		slog.Error("notification failed", attrs...)
	} else {
		slog.Info("notification sent", "channel", channel, "kind", kind)
	}
}
