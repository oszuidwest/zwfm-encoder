package dante

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

type blockedKeepaliveConn struct {
	net.PacketConn
	blocked net.Conn
}

func (c blockedKeepaliveConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.blocked.Write(p)
}

func (c blockedKeepaliveConn) SetWriteDeadline(deadline time.Time) error {
	return c.blocked.SetWriteDeadline(deadline)
}

func (c blockedKeepaliveConn) Close() error {
	return errors.Join(c.blocked.Close(), c.PacketConn.Close())
}

func TestBlockedKeepaliveWriteEndsReceiver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeTransmitter()
		defer h.close(t)
		h.start(t)
		blocked, peer := net.Pipe()
		defer func() { _ = blocked.Close(); _ = peer.Close() }()
		listen := listenDanteUDP
		listenDanteUDP = func(local *net.UDPAddr) (net.PacketConn, error) {
			conn, err := listen(local)
			if err == nil && !local.IP.IsUnspecified() {
				return blockedKeepaliveConn{PacketConn: conn, blocked: blocked}, nil
			}
			return conn, err
		}
		r, err := Open(t.Context(), testInput)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		watchdog := time.AfterFunc(10*time.Second, func() { _ = blocked.Close() })
		defer watchdog.Stop()
		flow := <-h.flows
		if _, err := h.media.WriteTo(mediaDatagram(0, []byte{1, 2, 3, 4}), flow.target); err != nil {
			t.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, r); readDone <- err }()
		started := time.Now()
		if err := r.Wait(); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(started) != 2*keepaliveInterval {
			t.Fatalf("Wait = %v after %s, want keepalive write timeout after %s", err, time.Since(started), 2*keepaliveInterval)
		}
		if err := <-readDone; !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v, want keepalive write error", err)
		}
		assertStoppedFlow(t, h)
	})
}
