package notify

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// RunHeartbeat pings url each interval until ctx is cancelled; empty disables it.
func RunHeartbeat(ctx context.Context, url string, interval time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if url == "" || err != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		resp, err := webhookClient.Do(req)
		failed := err != nil
		if err == nil {
			failed = resp.StatusCode < 200 || resp.StatusCode >= 300
			_ = resp.Body.Close()
		}
		if failed && !failing && ctx.Err() == nil {
			// HTTP errors can contain the secret URL, including redirect targets.
			slog.Warn("heartbeat ping failed")
		}
		failing = failed
	}
}
