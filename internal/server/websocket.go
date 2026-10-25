package server

import (
	"io"
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

// upgrader uses Gorilla's default origin check: Origin must match Host,
// including the port.
var upgrader = websocket.Upgrader{}

// UpgradeConnection upgrades an HTTP connection to WebSocket.
func UpgradeConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return upgrader.Upgrade(w, r, nil)
}
