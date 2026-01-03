package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

// WebSocketConn is the interface for WebSocket connection operations.
type WebSocketConn interface {
	io.Closer
	WriteJSON(v interface{}) error
	ReadJSON(v interface{}) error
}

var upgrader = websocket.Upgrader{
	CheckOrigin: checkOrigin,
}

// checkOrigin reports whether the WebSocket connection origin is allowed.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	// Same-origin requests omit the Origin header
	if origin == "" {
		return true
	}

	u, err := url.Parse(origin)
	if err != nil {
		slog.Warn("rejected WebSocket connection: invalid origin URL", "origin", origin)
		return false
	}

	host := u.Hostname()

	// Exact localhost matches
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}

	// Same-origin check (compare with request host)
	requestHost := r.Host
	// Strip port from request host for comparison
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		requestHost = h
	}
	if host == requestHost {
		return true
	}

	// Check private IP ranges using net.IP
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return true
	}

	slog.Warn("rejected WebSocket connection", "origin", origin, "host", host)
	return false
}

// UpgradeConnection upgrades an HTTP connection to WebSocket.
func UpgradeConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return upgrader.Upgrade(w, r, nil)
}
