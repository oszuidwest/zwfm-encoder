package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestUpgradeConnectionRequiresSameOrigin(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := UpgradeConnection(w, r); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	for origin, want := range map[string]int{
		"":                    http.StatusSwitchingProtocols, // A non-browser client.
		"http://" + host:      http.StatusSwitchingProtocols,
		"http://127.0.0.1:1":  http.StatusForbidden, // Another port of the same host.
		"http://192.168.1.20": http.StatusForbidden, // Another device on the LAN.
	} {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial("ws://"+host, header)
		if err == nil {
			_ = conn.Close()
		}
		if resp == nil || resp.StatusCode != want {
			t.Errorf("origin %q: response %v, want status %d", origin, resp, want)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
}
