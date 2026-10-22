package notify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		defer slog.SetDefault(previous)
		transport := webhookClient.Transport
		defer func() { webhookClient.Transport = transport }()
		status, calls := http.StatusNoContent, 0
		location := ""
		var requestErr error
		const url = "https://heartbeat.example/secret-token"
		webhookClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if req.Method != http.MethodGet || req.URL.String() != url {
				t.Errorf("unexpected request: %s %s", req.Method, req.URL)
				return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
			}
			if requestErr != nil {
				return nil, requestErr
			}
			return &http.Response{StatusCode: status, Body: http.NoBody, Header: http.Header{"Location": {location}}}, nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go RunHeartbeat(ctx, "", time.Minute)
		go RunHeartbeat(ctx, url, time.Minute)
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		tick := func(delay time.Duration, wantCalls, wantWarnings int) {
			t.Helper()
			mu.Unlock() // Let the heartbeat use the test state while time advances.
			time.Sleep(delay)
			synctest.Wait()
			mu.Lock()
			if calls != wantCalls || strings.Count(logs.String(), "level=WARN") != wantWarnings || strings.Contains(logs.String(), "secret-token") {
				t.Fatalf("calls=%d, want %d; want warnings=%d; logs=%s", calls, wantCalls, wantWarnings, logs.String())
			}
		}
		tick(time.Minute-time.Nanosecond, 0, 0)
		tick(time.Nanosecond, 1, 0)
		status = http.StatusServiceUnavailable
		tick(time.Minute, 2, 1)
		tick(10*time.Minute, 12, 1)
		status = http.StatusNoContent
		tick(time.Minute, 13, 1)
		status, location = http.StatusFound, "https://elsewhere.example/secret-token"
		tick(time.Minute, 14, 2)
		status = http.StatusNoContent
		tick(time.Minute, 15, 2)
		// net/http errors quote malformed redirect Locations.
		status, location = http.StatusFound, url+"%zz"
		tick(time.Minute, 16, 3)
		status, location = http.StatusNoContent, ""
		tick(time.Minute, 17, 3)
		requestErr = fmt.Errorf("dial failed: %s", url)
		tick(time.Minute, 18, 4)
		tick(time.Minute, 19, 4)
		cancel()
		tick(time.Minute, 19, 4)
	})
}
