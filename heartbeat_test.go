package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testHeartbeatInterval = 10 * time.Millisecond

func pingCounter(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, &n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeartbeatPingsEveryInterval(t *testing.T) {
	srv, pings := pingCounter(t)
	hb := NewHeartbeat(srv.URL, testHeartbeatInterval)
	defer hb.Stop()

	waitFor(t, "three pings", func() bool { return pings.Load() >= 3 })
}

func TestHeartbeatFirstPingWaitsOneInterval(t *testing.T) {
	srv, pings := pingCounter(t)
	hb := NewHeartbeat(srv.URL, time.Hour)
	time.Sleep(50 * time.Millisecond)
	hb.Stop()

	if n := pings.Load(); n != 0 {
		t.Fatalf("pings before the first interval = %d, want 0", n)
	}
}

func TestHeartbeatDisabledWithoutURL(t *testing.T) {
	hb := NewHeartbeat("", testHeartbeatInterval)
	if hb != nil {
		t.Fatal("NewHeartbeat without URL started a loop")
	}
	hb.Stop() // Must be safe on the nil Heartbeat a disabled config yields.
}

func TestHeartbeatStopAbortsInFlightPing(t *testing.T) {
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	hb := NewHeartbeat(srv.URL, testHeartbeatInterval)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		hb.Stop()
		t.Fatal("no ping reached the server")
	}

	stopped := make(chan struct{})
	go func() {
		hb.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for the in-flight ping")
	}
}
