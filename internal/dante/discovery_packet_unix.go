//go:build !windows

package dante

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

func discoveryPacketReader(conn *net.UDPConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
	packetConn := ipv4.NewPacketConn(conn)
	if err := packetConn.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return nil, err
	}
	return func(buffer []byte) (int, *net.UDPAddr, int, error) {
		n, control, address, err := packetConn.ReadFrom(buffer)
		if err != nil {
			return n, nil, 0, err
		}
		source, ok := address.(*net.UDPAddr)
		if !ok {
			return n, nil, 0, fmt.Errorf("unexpected discovery source %T", address)
		}
		if control == nil {
			return n, source, 0, nil
		}
		return n, source, control.IfIndex, nil
	}, nil
}
