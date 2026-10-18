package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

// TextMessage is the WebSocket text message type for WriteMessage.
const TextMessage = websocket.TextMessage

// WebSocketConn is the interface for WebSocket connection operations.
type WebSocketConn interface {
	io.Closer
	// WriteJSON writes a JSON message to the connection.
	WriteJSON(v any) error
	// WriteMessage writes a raw message of the given type to the connection.
	WriteMessage(messageType int, data []byte) error
	// ReadMessage reads the next message from the connection.
	ReadMessage() (messageType int, p []byte, err error)
}

// checkOrigin reports whether the WebSocket connection origin is allowed.
func checkOrigin(r *http.Request, trustedProxies []string) bool {
	origins := r.Header.Values("Origin")
	// Non-browser clients may omit Origin; session authentication still applies.
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Only the explicitly trusted immediate peer may describe the external
	// scheme. Host (including its external port) must be preserved by the proxy.
	if forwarded := r.Header.Values("X-Forwarded-Proto"); len(forwarded) > 0 && isTrustedProxy(loginClientAddress(r, nil), trustedProxies) {
		if len(forwarded) != 1 || (forwarded[0] != "http" && forwarded[0] != "https") {
			return false
		}
		scheme = forwarded[0]
	}
	origin, ok := webOrigin(origins[0])
	target, targetOK := webOrigin(scheme + "://" + r.Host)
	if ok && targetOK && origin == target {
		return true
	}
	//nolint:gosec // G706: origin/host are logged as structured slog values for security auditing
	slog.Warn("rejected WebSocket connection: origin mismatch", "origin", origins[0], "host", r.Host)
	return false
}

// webOrigin compares the RFC6454 scheme/host/effective-port tuple, not a DNS
// destination or a hostname-only match. Private/loopback addresses have no exemption.
func webOrigin(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || u.Path != "" || strings.ContainsAny(value, "?#") {
		return "", false
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if host == "" || (scheme != "http" && scheme != "https") || strings.Contains(host, "%") {
		return "", false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", false
		}
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", false
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.FormatUint(number, 10)), true
}

// UpgradeConnection upgrades an HTTP connection to WebSocket.
func UpgradeConnection(w http.ResponseWriter, r *http.Request, trustedProxies []string) (*websocket.Conn, error) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return checkOrigin(r, trustedProxies) }}
	return upgrader.Upgrade(w, r, nil)
}
