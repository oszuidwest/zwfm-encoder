package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/notify"
)

// Heartbeat pings a push monitor every interval while the process runs, so the
// monitor goes down when the host, its network or the encoder is down. Audio
// problems are left to the silence and stream alerts.
type Heartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewHeartbeat starts pinging url, or returns nil when url is empty.
// Stop must be called on shutdown.
func NewHeartbeat(url string, interval time.Duration) *Heartbeat {
	if url == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	hb := &Heartbeat{cancel: cancel, done: make(chan struct{})}
	go hb.run(ctx, url, interval)
	return hb
}

// Stop ends the loop and aborts an in-flight ping. It is a no-op on a nil Heartbeat.
func (hb *Heartbeat) Stop() {
	if hb == nil {
		return
	}
	hb.cancel()
	<-hb.done
}

func (hb *Heartbeat) run(ctx context.Context, url string, interval time.Duration) {
	defer close(hb.done)

	// Wait one full interval before the first ping.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failing := false
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
		err := notify.SendHeartbeat(ctx, url)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && !failing:
			slog.Warn("heartbeat ping failed", "error", err)
		case err == nil && failing:
			slog.Info("heartbeat ping recovered")
		}
		failing = err != nil
	}
}
