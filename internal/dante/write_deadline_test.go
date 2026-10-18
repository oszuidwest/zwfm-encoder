package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestControlBlockedWriteIsBounded(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		name := "timeout"
		if cancelEarly {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, peer := net.Pipe()
				defer func() { _ = client.Close(); _ = peer.Close() }()
				// The peer never reads. This watchdog makes the unfixed code fail promptly.
				watchdog := time.AfterFunc(10*time.Second, func() { _ = client.Close() })
				defer watchdog.Stop()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if cancelEarly {
					time.AfterFunc(20*time.Millisecond, cancel)
				}
				started := time.Now()
				_, err := exchangeControl(ctx, client, buildStopRequest([6]byte{}), time.Second)
				want, elapsed := os.ErrDeadlineExceeded, time.Second
				if cancelEarly {
					want, elapsed = context.Canceled, 20*time.Millisecond
				}
				if !errors.Is(err, want) || time.Since(started) != elapsed {
					t.Fatalf("blocked write = %v after %s, want %v after %s", err, time.Since(started), want, elapsed)
				}
			})
		})
	}
}

// Only stop-flow writes stall; setup still uses the deterministic transmitter.
type blockedStopConn struct {
	net.Conn
	blocked net.Conn
}

func (c blockedStopConn) Write(p []byte) (int, error) {
	if binary.BigEndian.Uint16(p[6:8]) == stopFlowOpcode {
		return c.blocked.Write(p)
	}
	return c.Conn.Write(p)
}

func (c blockedStopConn) SetDeadline(deadline time.Time) error {
	if err := c.blocked.SetDeadline(deadline); err != nil {
		return err
	}
	return c.Conn.SetDeadline(deadline)
}

func TestReceiverWaitReturnsWithBlockedStopFlowWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeTransmitter()
		defer h.close(t)
		h.start(t)
		blocked, peer := net.Pipe()
		defer func() { _ = blocked.Close(); _ = peer.Close() }()
		dial := dialDanteUDP
		dialDanteUDP = func(local, remote *net.UDPAddr) (net.Conn, error) {
			conn, err := dial(local, remote)
			if err == nil && remote.Port == int(h.channels[0].port) {
				return blockedStopConn{Conn: conn, blocked: blocked}, nil
			}
			return conn, err
		}
		r, err := Open(t.Context(), testInput)
		if err != nil {
			t.Fatal(err)
		}
		watchdog := time.AfterFunc(10*time.Second, func() { _ = blocked.Close() })
		defer watchdog.Stop()
		started := time.Now()
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if err := r.Wait(); err != nil || time.Since(started) != time.Second {
			t.Fatalf("Wait = %v after %s, want bounded one-second stop-flow timeout", err, time.Since(started))
		}
	})
}

type blockedQueryConn struct {
	net.PacketConn
	blocked net.Conn
}

func (c blockedQueryConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.blocked.Write(p)
}

func (c blockedQueryConn) SetDeadline(deadline time.Time) error {
	if err := c.blocked.SetDeadline(deadline); err != nil {
		return err
	}
	return c.PacketConn.SetDeadline(deadline)
}

func (c blockedQueryConn) Close() error {
	return errors.Join(c.blocked.Close(), c.PacketConn.Close())
}

func TestDiscoveryBlockedWriteIsBounded(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		name := "timeout"
		if cancelEarly {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.start(t)
				blocked, peer := net.Pipe()
				defer func() { _ = blocked.Close(); _ = peer.Close() }()
				listen := listenDanteUDP
				listenDanteUDP = func(local *net.UDPAddr) (net.PacketConn, error) {
					conn, err := listen(local)
					return blockedQueryConn{PacketConn: conn, blocked: blocked}, err
				}
				watchdog := time.AfterFunc(10*time.Second, func() { _ = blocked.Close() })
				defer watchdog.Stop()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if cancelEarly {
					time.AfterFunc(20*time.Millisecond, cancel)
				}
				started := time.Now()
				r, err := Open(ctx, testInput)
				if r != nil {
					_ = r.Close()
					t.Fatal("Open succeeded despite blocked discovery")
				}
				elapsed := 4 * time.Second
				if cancelEarly {
					elapsed = 20 * time.Millisecond
					if !errors.Is(err, context.Canceled) {
						t.Errorf("Open error = %v, want cancellation", err)
					}
				}
				if err == nil || time.Since(started) != elapsed {
					t.Fatalf("blocked discovery = %v after %s, want error after %s", err, time.Since(started), elapsed)
				}
			})
		})
	}
}
