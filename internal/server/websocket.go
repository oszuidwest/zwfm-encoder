package server

import (
	"io"
	"log/slog"
	"net/http"

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

var (
	sameOrigin = http.NewCrossOriginProtection()
	upgrader   = websocket.Upgrader{CheckOrigin: checkOrigin}
)

// checkOrigin applies the cross-origin policy that already guards every unsafe
// request to the handshake, which CrossOriginProtection exempts as a GET.
func checkOrigin(r *http.Request) bool {
	handshake := *r
	handshake.Method = http.MethodPost
	if err := sameOrigin.Check(&handshake); err != nil {
		//nolint:gosec // G706: origin/host are logged as structured slog values for security auditing
		slog.Warn("rejected WebSocket connection", "origin", r.Header.Get("Origin"), "host", r.Host, "error", err)
		return false
	}
	return true
}

// UpgradeConnection upgrades an HTTP connection to WebSocket.
func UpgradeConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return upgrader.Upgrade(w, r, nil)
}
