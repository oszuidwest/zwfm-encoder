package dante

import (
	"errors"
	"net"
	"os"
	"testing"
)

func listenLoopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err == nil {
		return conn
	}
	if errors.Is(err, os.ErrPermission) {
		t.Skipf("sandbox does not permit loopback UDP sockets: %v", err)
	}
	t.Fatalf("open loopback UDP socket: %v", err)
	return nil
}
