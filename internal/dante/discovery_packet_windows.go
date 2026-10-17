package dante

import (
	"net"
	"strconv"

	"golang.org/x/sys/windows"
)

func discoveryPacketReader(conn *net.UDPConn) (func([]byte) (int, *net.UDPAddr, int, error), error) {
	// x/net/ipv4 cannot enable receive control on Windows; use Winsock IP_PKTINFO.
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		optionErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_PKTINFO, 1)
	}); err != nil {
		return nil, err
	}
	if optionErr != nil {
		return nil, optionErr
	}
	control := make([]byte, 128)
	return func(buffer []byte) (int, *net.UDPAddr, int, error) {
		n, controlLen, flags, source, err := conn.ReadMsgUDP(buffer, control)
		if err != nil || flags&windows.MSG_CTRUNC != 0 {
			return n, source, 0, err
		}
		return n, source, windowsPacketInterface(control[:controlLen], strconv.IntSize/8), nil
	}, nil
}
