package dante

import (
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestDiscoveryReadsReceivingInterface(t *testing.T) {
	t.Parallel()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	readPacket, err := discoveryPacketReader(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected UDP listener")
	}
	if _, err := conn.WriteToUDP([]byte("discovery"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: local.Port}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 100)
	n, source, index, err := readPacket(buffer)
	if err != nil || source == nil || !source.IP.IsLoopback() || string(buffer[:n]) != "discovery" {
		t.Fatalf("readPacket = %q, %v, interface %d, %v", buffer[:n], source, index, err)
	}
	iface, err := net.InterfaceByIndex(index)
	if err != nil || iface.Flags&net.FlagLoopback == 0 {
		t.Fatalf("received on interface %d (%v), %v; want loopback", index, iface, err)
	}
}

func TestWindowsPacketInterface(t *testing.T) {
	t.Parallel()
	for _, wordSize := range []int{4, 8} {
		t.Run(strconv.Itoa(wordSize*8)+" bit", func(t *testing.T) {
			t.Parallel()
			alignment := uint32(4)
			if wordSize == 8 {
				alignment = 8
			}
			headerSize := alignment + 8
			packetInfo := make([]byte, headerSize+8)
			binary.LittleEndian.PutUint32(packetInfo, headerSize+8)
			binary.LittleEndian.PutUint32(packetInfo[wordSize+4:], 19)
			copy(packetInfo[headerSize:], []byte{127, 0, 0, 1})
			binary.LittleEndian.PutUint32(packetInfo[headerSize+4:], 7)
			unrelated := make([]byte, (headerSize+alignment)&^(alignment-1))
			binary.LittleEndian.PutUint32(unrelated, headerSize+1)
			binary.LittleEndian.PutUint32(unrelated[wordSize:], 41)
			for _, tt := range []struct {
				name string
				data []byte
				want int
			}{
				{name: "packet info", data: packetInfo, want: 7},
				{name: "preceding aligned control", data: append(unrelated, packetInfo...), want: 7},
				{name: "truncated header", data: packetInfo[:headerSize-1]},
				{name: "truncated data", data: packetInfo[:len(packetInfo)-1]},
				{name: "unrelated control", data: unrelated},
				{name: "zero length", data: make([]byte, headerSize)},
			} {
				t.Run(tt.name, func(t *testing.T) {
					if got := windowsPacketInterface(tt.data, wordSize); got != tt.want {
						t.Fatalf("interface = %d, want %d", got, tt.want)
					}
				})
			}
			shortInfo := append([]byte(nil), packetInfo[:headerSize+4]...)
			binary.LittleEndian.PutUint32(shortInfo, headerSize+4)
			if got := windowsPacketInterface(shortInfo, wordSize); got != 0 {
				t.Fatalf("short IN_PKTINFO interface = %d", got)
			}
		})
	}
}
