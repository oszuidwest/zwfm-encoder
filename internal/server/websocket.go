package server

import (
	"net/http"

	"github.com/coder/websocket"
)

// UpgradeConnection upgrades an HTTP connection to WebSocket. It uses the
// library's default origin check: Origin must match Host, including the port.
func UpgradeConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, nil)
}
