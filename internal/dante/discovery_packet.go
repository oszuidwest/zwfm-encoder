package dante

import "encoding/binary"

// windowsPacketInterface reads Winsock's WSACMSGHDR + IN_PKTINFO on either word size.
// https://learn.microsoft.com/en-us/windows/win32/api/ws2ipdef/ns-ws2ipdef-in_pktinfo
func windowsPacketInterface(control []byte, wordSize int) int {
	if wordSize != 4 && wordSize != 8 {
		return 0
	}
	alignment := uint64(4)
	if wordSize == 8 {
		alignment = 8
	}
	headerSize := alignment + 8
	for uint64(len(control)) >= headerSize {
		length := uint64(binary.LittleEndian.Uint32(control))
		if wordSize == 8 {
			length = binary.LittleEndian.Uint64(control)
		}
		if length < headerSize || length > uint64(len(control)) {
			return 0
		}
		level := binary.LittleEndian.Uint32(control[wordSize:])
		kind := binary.LittleEndian.Uint32(control[wordSize+4:])
		if level == 0 && kind == 19 { // Winsock IPPROTO_IP / IP_PKTINFO.
			if length < headerSize+8 {
				return 0
			}
			index := binary.LittleEndian.Uint32(control[headerSize+4:])
			if uint64(index) > uint64(^uint(0)>>1) {
				return 0
			}
			return int(index)
		}
		next := (length + alignment - 1) & ^(alignment - 1)
		if next > uint64(len(control)) {
			return 0
		}
		control = control[next:]
	}
	return 0
}
