package dante

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// lateDeadlineConn fails its second SetReadDeadline once the lookup has expired.
type lateDeadlineConn struct {
	net.PacketConn
	deadline time.Time
	calls    int
}

func (c *lateDeadlineConn) SetDeadline(d time.Time) error { c.deadline = d; return nil }

func (c *lateDeadlineConn) SetReadDeadline(time.Time) error {
	c.calls++
	if c.calls == 1 {
		return nil
	}
	time.Sleep(time.Until(c.deadline) + time.Nanosecond)
	return net.ErrClosed
}

func TestLookupTimeoutKeepsMalformedResponseAndTransportErrors(t *testing.T) {
	listen, query, read := listenDanteUDP, queryDante, readDanteDiscovery
	t.Cleanup(func() { listenDanteUDP, queryDante, readDanteDiscovery = listen, query, read })
	synctest.Test(t, func(t *testing.T) {
		h := newFakeTransmitter()
		defer h.close(t)
		listenDanteUDP = func(a *net.UDPAddr) (net.PacketConn, error) {
			c, err := listen(a)
			return &lateDeadlineConn{PacketConn: c}, err
		}
		queryDante = func(net.PacketConn, []net.Interface, []byte, *net.UDPAddr) error { return nil }
		readDanteDiscovery = func(net.PacketConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
			return func(b []byte) (int, *net.UDPAddr, int, error) {
				b[0] = 0xff // Malformed.
				return 1, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 5353}, 7, nil
			}, nil
		}
		if _, err := discoverChannel(t.Context(), h.interfaces, "tx", "Left", nil); err == nil || !strings.Contains(err.Error(), "malformed response") {
			t.Fatalf("timeout at SetReadDeadline lost the malformed-response diagnostic: %v", err)
		}
		listenDanteUDP = listen
		reset := errors.New("connection reset")
		queryDante = func(net.PacketConn, []net.Interface, []byte, *net.UDPAddr) error {
			time.Sleep(4*time.Second + time.Nanosecond)
			return reset
		}
		if _, err := discoverChannel(context.Background(), h.interfaces, "tx", "Left", nil); !errors.Is(err, reset) {
			t.Fatalf("transport error at lookup expiry reported as a timeout: %v", err)
		}
	})
}
