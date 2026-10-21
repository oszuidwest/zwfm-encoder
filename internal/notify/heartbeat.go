package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// SendHeartbeat sends one GET to a push monitor such as an UptimeRobot heartbeat.
// Errors never contain the URL: its path carries the monitor's secret token.
func SendHeartbeat(ctx context.Context, heartbeatURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, heartbeatURL, http.NoBody)
	if err != nil {
		return errors.New("invalid heartbeat URL")
	}
	resp, err := webhookClient.Do(req)
	if err != nil {
		return errors.New("heartbeat request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat returned status %d", resp.StatusCode)
	}
	return nil
}
