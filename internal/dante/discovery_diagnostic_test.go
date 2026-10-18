package dante

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"testing/synctest"
)

func TestTimeoutKeepsRelevantRejectionAfterUnrelatedInvalidSource(t *testing.T) {
	for _, mode := range []string{"wrong port", "off subnet"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.dropChannel = "Left"
				h.start(t)
				relevant := buildTestDNSResponse(t, []testChannel{{
					"Left@tx" + serviceSuffix, 4455,
					[]string{"id=bad", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"},
				}}, nil)
				unrelated := buildMixedDNSResponse(t, "Other@tx"+serviceSuffix, "Another@tx"+serviceSuffix)
				if port, txt, err := parseDNSResponse(unrelated, "Left@tx"+serviceSuffix); err != nil || port != 0 || txt != nil {
					t.Fatalf("unrelated fixture: port=%d, txt=%v, err=%v", port, txt, err)
				}
				read := readDanteDiscovery
				t.Cleanup(func() { readDanteDiscovery = read })
				readDanteDiscovery = func(conn net.PacketConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
					next, err := read(conn)
					calls := 0
					return func(p []byte) (int, *net.UDPAddr, int, error) {
						calls++
						source := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 5353}
						switch calls {
						case 1:
							return copy(p, relevant), source, 7, nil
						case 2:
							if mode == "wrong port" {
								source.Port = 12345
							} else {
								source.IP = net.IPv4(169, 254, 1, 2)
							}
							return copy(p, unrelated), source, 7, nil
						default:
							return next(p)
						}
					}, err
				}
				receiver, err := Open(t.Context(), testInput)
				if receiver != nil {
					_ = receiver.Close()
				}
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "invalid TXT id") {
					t.Fatalf("Open = %v; want deadline and original invalid TXT id rejection", err)
				}
			})
		})
	}
}
