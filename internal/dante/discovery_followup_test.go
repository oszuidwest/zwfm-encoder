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

func TestDiscoveryTimeoutIncludesRejectedSource(t *testing.T) {
	for _, mode := range []string{"off subnet", "wrong port", "missing interface", "caller deadline", "different transmitter"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.start(t)
				read := readDanteDiscovery
				readDanteDiscovery = func(conn net.PacketConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
					next, err := read(conn)
					right := h.readerCount == 2
					return func(p []byte) (int, *net.UDPAddr, int, error) {
						n, source, index, err := next(p)
						if err == nil {
							copySource := *source
							source = &copySource
							switch mode {
							case "different transmitter":
								if right {
									source.IP = net.IPv4(192, 0, 2, 21)
								}
							case "wrong port":
								source.Port = 12345
							case "missing interface":
								index = 0
							default:
								source.IP = net.IPv4(169, 254, 1, 2)
							}
						}
						return n, source, index, err
					}, err
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if mode == "caller deadline" {
					ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
					defer cancel()
				}
				_, err := Open(ctx, testInput)
				want := "169.254.1.2:5353"
				reason := "subnet"
				switch mode {
				case "different transmitter":
					want, reason = "192.0.2.21:5353", "does not match required 192.0.2.20"
				case "wrong port":
					want, reason = "192.0.2.20:12345", "port"
				case "missing interface":
					want, reason = "192.0.2.20:5353", "interface"
				}
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), reason) {
					t.Fatalf("Open = %v, want rejected source %s and reason %s", err, want, reason)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Open = %v, want timeout cause preserved", err)
				}
			})
		})
	}
}

func TestOpenUsesSelectedInterfaceAddressOnTransmitterSubnet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeTransmitter()
		defer h.close(t)
		h.addresses = append([]net.Addr{&net.IPNet{IP: net.IPv4(169, 254, 1, 2), Mask: net.CIDRMask(16, 32)}}, h.addresses...)
		h.start(t)
		r, err := Open(t.Context(), testInput+"?interface=test0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		for _, conn := range []interface{ LocalAddr() net.Addr }{r.mediaConn, r.controlConn} {
			if got := conn.LocalAddr().(*net.UDPAddr).IP; !got.Equal(net.IPv4(192, 0, 2, 10)) {
				t.Fatalf("bound address = %s, want address on transmitter subnet", got)
			}
		}
	})
}

func TestSelectedInterfaceWithoutTransmitterSubnetFails(t *testing.T) {
	h := newFakeTransmitter()
	defer h.close(t)
	_, err := selectLocalAddress("test0", h.interfaces, &net.UDPAddr{IP: net.IPv4(169, 254, 1, 2)})
	if err == nil || !strings.Contains(err.Error(), "no IPv4 address on transmitter") {
		t.Fatalf("selectLocalAddress = %v, want explicit subnet error", err)
	}
}
