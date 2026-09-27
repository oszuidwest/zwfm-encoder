package dante

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestMakeFlowRequestBody(t *testing.T) {
	t.Parallel()

	body, err := makeFlowRequestBody(&flowRequest{
		localIP:       net.IPv4(192, 0, 2, 20),
		mediaPort:     50000,
		receiverName:  "Encoder",
		flowName:      "encoder_1",
		sampleRate:    48000,
		bitsPerSample: 24,
		fpp:           32,
		channelIDs:    []uint16{7, 8},
	})
	if err != nil {
		t.Fatalf("makeFlowRequestBody() error = %v", err)
	}

	stringsOffset := int(binary.BigEndian.Uint16(body[0:2]))
	if stringsOffset != 52 {
		t.Fatalf("strings offset = %d, want 52", stringsOffset)
	}
	if got := binary.BigEndian.Uint32(body[2:6]); got != 48000 {
		t.Fatalf("sample rate = %d, want 48000", got)
	}
	if got := binary.BigEndian.Uint32(body[6:10]); got != 24 {
		t.Fatalf("sample depth = %d, want 24", got)
	}
	if got := binary.BigEndian.Uint16(body[12:14]); got != 2 {
		t.Fatalf("channel count = %d, want 2", got)
	}
	if got := [2]uint16{binary.BigEndian.Uint16(body[16:18]), binary.BigEndian.Uint16(body[18:20])}; got != [2]uint16{7, 8} {
		t.Fatalf("channel IDs = %v, want [7 8]", got)
	}
	if got := binary.BigEndian.Uint16(body[26:28]); got != 32 {
		t.Fatalf("frames per packet = %d, want 32", got)
	}
	stringStart := stringsOffset - requestHeaderLength
	if got := string(body[stringStart : stringStart+8]); got != "Encoder\x00" {
		t.Fatalf("receiver name = %q, want %q", got, "Encoder\\x00")
	}
	addressOffset := int(binary.BigEndian.Uint16(body[14:16])) - requestHeaderLength
	if got := binary.BigEndian.Uint16(body[addressOffset+2 : addressOffset+4]); got != 50000 {
		t.Fatalf("media port = %d, want 50000", got)
	}
	if got := net.IP(body[addressOffset+4 : addressOffset+8]); !got.Equal(net.IPv4(192, 0, 2, 20)) {
		t.Fatalf("media address = %v, want 192.0.2.20", got)
	}
}

func TestRequestFlow(t *testing.T) {
	t.Parallel()

	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer func() {
		_ = server.Close()
	}()

	wantHandle := flowHandle{1, 2, 3, 4, 5, 6}
	serverDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1500)
		size, client, readErr := server.ReadFromUDP(buffer)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		sequence := binary.BigEndian.Uint16(buffer[4:6])
		opcode := binary.BigEndian.Uint16(buffer[6:8])
		response := makeControlPacket(flowStartCode, sequence, opcode, responseOK, wantHandle[:])
		_, writeErr := server.WriteToUDP(response, client)
		if size < requestHeaderLength || binary.BigEndian.Uint16(buffer[0:2]) != flowStartCode {
			t.Errorf("invalid flow request packet: %x", buffer[:size])
		}
		serverDone <- writeErr
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := flowRequest{
		localIP:       net.IPv4(127, 0, 0, 1),
		mediaPort:     50000,
		receiverName:  "Encoder",
		flowName:      "encoder_1",
		sampleRate:    48000,
		bitsPerSample: 24,
		fpp:           32,
		channelIDs:    []uint16{1, 2},
	}
	got, err := requestFlow(ctx, server.LocalAddr().(*net.UDPAddr), flowStartCode, &request)
	if err != nil {
		t.Fatalf("requestFlow() error = %v", err)
	}
	if got != wantHandle {
		t.Fatalf("requestFlow() = %v, want %v", got, wantHandle)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fake transmitter error = %v", err)
	}
}

func TestExchangeControlPacketHonorsCancellation(t *testing.T) {
	t.Parallel()

	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer func() {
		_ = server.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()
	_, err = exchangeControlPacket(ctx, controlExchange{
		localIP:   net.IPv4(127, 0, 0, 1),
		remote:    server.LocalAddr().(*net.UDPAddr),
		startCode: flowStartCode,
		sequence:  1,
		opcode:    requestFlowOpcode,
	})
	if err == nil {
		t.Fatal("exchangeControlPacket() error = nil")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("exchangeControlPacket() cancellation took %v", elapsed)
	}
}
