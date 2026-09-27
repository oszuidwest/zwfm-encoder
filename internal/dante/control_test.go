package dante

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBuildFlowRequest(t *testing.T) {
	t.Parallel()
	parameters := flowParameters{
		bits:       24,
		channelIDs: [mediaChannels]uint16{0x0102, 0x0304},
		fpp:        32,
		receiver:   "receiver",
		mediaPort:  5000,
		localIP:    net.IPv4(192, 0, 2, 10),
	}
	message := buildFlowRequest(&parameters)
	if got := binary.BigEndian.Uint16(message[0:2]); got != 0x1102 {
		t.Errorf("start code = %#x", got)
	}
	if got := int(binary.BigEndian.Uint16(message[2:4])); got != len(message) {
		t.Errorf("length field = %d, actual = %d", got, len(message))
	}
	if got := binary.BigEndian.Uint16(message[4:6]); got != requestFlowSequence {
		t.Errorf("sequence = %#x", got)
	}
	if got := binary.BigEndian.Uint16(message[6:8]); got != requestFlowOpcode {
		t.Errorf("opcode = %#x", got)
	}
	if got := binary.BigEndian.Uint16(message[8:10]); got != 0 {
		t.Errorf("status = %#x", got)
	}

	body := message[controlHeaderSize:]
	stringOffset := int(binary.BigEndian.Uint16(body[0:2]))
	if stringOffset != 52 {
		t.Errorf("string offset = %d, want 52", stringOffset)
	}
	if got := binary.BigEndian.Uint32(body[2:6]); got != 48000 {
		t.Errorf("sample rate = %d", got)
	}
	if got := binary.BigEndian.Uint32(body[6:10]); got != 24 {
		t.Errorf("bits = %d", got)
	}
	checkUint16(t, body, 10, 0x0001, "constant one")
	checkUint16(t, body, 12, 2, "channel count")
	addressOffset := int(binary.BigEndian.Uint16(body[14:16]))
	checkUint16(t, body, 16, 0x0102, "left ID")
	checkUint16(t, body, 18, 0x0304, "right ID")
	checkUint16(t, body, 20, 0x0020, "body length constant")
	checkUint16(t, body, 22, 0x0a00, "0x0a00 constant")
	checkUint16(t, body, 24, 0x0002, "0x0002 constant")
	checkUint16(t, body, 26, 32, "frames per packet")
	flowOffset := int(binary.BigEndian.Uint16(body[28:30]))
	for offset, value := range body[30:42] {
		if value != 0 {
			t.Errorf("reserved byte at body offset %d = %#x", offset+30, value)
		}
	}
	if got := string(message[stringOffset : stringOffset+len("receiver")+1]); got != "receiver\x00" {
		t.Errorf("receiver string = %q", got)
	}
	if got := string(message[flowOffset : flowOffset+len("encoder_1")+1]); got != "encoder_1\x00" {
		t.Errorf("flow string = %q", got)
	}
	if addressOffset%8 != 0 {
		t.Errorf("address offset %d is not 8-byte aligned", addressOffset)
	}
	for offset, value := range message[flowOffset+len("encoder_1")+1 : addressOffset] {
		if value != 0 {
			t.Errorf("padding byte %d = %#x", offset, value)
		}
	}
	checkUint16(t, message, addressOffset, 0x0802, "address type")
	checkUint16(t, message, addressOffset+2, 5000, "media port")
	if got := net.IP(message[addressOffset+4 : addressOffset+8]); !got.Equal(parameters.localIP) {
		t.Errorf("local IP = %v", got)
	}
}

func TestExchangeControl(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    uint16
		wantError string
		wantBody  string
	}{
		{name: "matching success after ignored datagrams", status: 1, wantBody: "result"},
		{name: "error code", status: 0x0314, wantError: "0x0314"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := listenLoopbackUDP(t)
			defer func() {
				_ = server.Close()
			}()
			client, err := net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = client.Close()
			}()

			const sequence = stopFlowSequence
			request := buildStopRequest([6]byte{})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				buffer := make([]byte, 64)
				_, peer, readErr := server.ReadFromUDP(buffer)
				if readErr != nil {
					return
				}
				_, _ = server.WriteToUDP([]byte{0, 1, 2}, peer)
				_, _ = server.WriteToUDP(controlResponse(sequence+1, stopFlowOpcode, 1, nil), peer)
				_, _ = server.WriteToUDP(controlResponse(sequence, stopFlowOpcode, test.status, []byte(test.wantBody)), peer)
			}()
			body, err := exchangeControl(context.Background(), client, request, time.Second)
			<-serverDone
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("exchangeControl error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("exchangeControl: %v", err)
			}
			if string(body) != test.wantBody {
				t.Fatalf("body = %q, want %q", body, test.wantBody)
			}
		})
	}
}

func checkUint16(t *testing.T, data []byte, offset int, want uint16, field string) {
	t.Helper()
	if got := binary.BigEndian.Uint16(data[offset : offset+2]); got != want {
		t.Errorf("%s = %#x, want %#x", field, got, want)
	}
}

func controlResponse(sequence, opcode, status uint16, body []byte) []byte {
	message := make([]byte, controlHeaderSize+len(body))
	binary.BigEndian.PutUint16(message[0:2], 0x1102)
	binary.BigEndian.PutUint16(message[2:4], uint16(len(message))) //nolint:gosec // test bodies are tiny.
	binary.BigEndian.PutUint16(message[4:6], sequence)
	binary.BigEndian.PutUint16(message[6:8], opcode)
	binary.BigEndian.PutUint16(message[8:10], status)
	copy(message[controlHeaderSize:], body)
	return message
}
