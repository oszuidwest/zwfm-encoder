package dante

import (
	"fmt"
	"net"
)

// Keep OS/network operations replaceable by an isolated packet network in tests.
var (
	listInterfaces     = net.Interfaces
	lookupInterface    = net.InterfaceByName
	interfaceAddresses = (*net.Interface).Addrs
	listenDanteUDP     = func(local *net.UDPAddr) (net.PacketConn, error) {
		return net.ListenUDP("udp4", local)
	}
	dialDanteUDP = func(local, remote *net.UDPAddr) (net.Conn, error) {
		return net.DialUDP("udp4", local, remote)
	}
	queryDante         = sendDiscoveryQuery
	readDanteDiscovery = func(conn net.PacketConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
		udp, ok := conn.(*net.UDPConn)
		if !ok {
			return nil, fmt.Errorf("unsupported discovery socket %T", conn)
		}
		return discoveryPacketReader(udp)
	}
)
