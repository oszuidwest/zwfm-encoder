package dante

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	controlHeaderSize   = 10
	requestFlowOpcode   = 0x0100
	stopFlowOpcode      = 0x0101
	requestFlowSequence = 1
	stopFlowSequence    = 2
	flowName            = "encoder_1"
)

type flowParameters struct {
	bits       uint16
	channelIDs [mediaChannels]uint16
	fpp        uint16
	receiver   string
	mediaPort  uint16
	localIP    net.IP
}

func chooseFPP(maximum, minimum, bytesPerSample uint16) (uint16, error) {
	budget := uint16(1400) / (mediaChannels * bytesPerSample)
	chosen := min(maximum, budget)
	if chosen < minimum {
		return 0, fmt.Errorf(
			"transmitter minimum of %d frames per packet exceeds the MTU budget of %d",
			minimum,
			budget,
		)
	}
	return chosen, nil
}

// receiverName returns the hostname reduced to at most 31 printable ASCII bytes.
func receiverName() string {
	name, _ := os.Hostname()
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return "ZWFM Encoder"
	}
	return name[:min(len(name), 31)]
}

func buildFlowRequest(parameters *flowParameters) []byte {
	const stringOffset uint16 = controlHeaderSize + 38 + 2*mediaChannels
	flowNameOffset := stringOffset + 1 + uint16(len(parameters.receiver))  //nolint:gosec // receiverName caps the name at 31 bytes.
	addressOffset := (flowNameOffset + uint16(len(flowName)) + 1 + 7) &^ 7 // 8-byte aligned
	totalLength := addressOffset + 8

	message := make([]byte, totalLength)
	binary.BigEndian.PutUint16(message[0:2], 0x1102)
	binary.BigEndian.PutUint16(message[2:4], totalLength)
	binary.BigEndian.PutUint16(message[4:6], requestFlowSequence)
	binary.BigEndian.PutUint16(message[6:8], requestFlowOpcode)

	body := message[controlHeaderSize:stringOffset]
	binary.BigEndian.PutUint16(body[0:2], stringOffset)
	binary.BigEndian.PutUint32(body[2:6], sampleRate)
	binary.BigEndian.PutUint32(body[6:10], uint32(parameters.bits))
	binary.BigEndian.PutUint16(body[10:12], 0x0001)
	binary.BigEndian.PutUint16(body[12:14], mediaChannels)
	binary.BigEndian.PutUint16(body[14:16], addressOffset)
	for index, id := range parameters.channelIDs {
		start := 16 + 2*index
		binary.BigEndian.PutUint16(body[start:start+2], id)
	}
	const afterChannels = 16 + 2*mediaChannels
	binary.BigEndian.PutUint16(body[afterChannels:afterChannels+2], 0x001c+2*mediaChannels)
	binary.BigEndian.PutUint16(body[afterChannels+2:afterChannels+4], 0x0a00)
	binary.BigEndian.PutUint16(body[afterChannels+4:afterChannels+6], 0x0002)
	binary.BigEndian.PutUint16(body[afterChannels+6:afterChannels+8], parameters.fpp)
	binary.BigEndian.PutUint16(body[afterChannels+8:afterChannels+10], flowNameOffset)

	copy(message[stringOffset:], parameters.receiver)
	copy(message[flowNameOffset:], flowName)
	binary.BigEndian.PutUint16(message[addressOffset:addressOffset+2], 0x0802)
	binary.BigEndian.PutUint16(message[addressOffset+2:addressOffset+4], parameters.mediaPort)
	copy(message[addressOffset+4:addressOffset+8], parameters.localIP.To4())
	return message
}

func buildStopRequest(handle [6]byte) []byte {
	message := make([]byte, controlHeaderSize+len(handle))
	binary.BigEndian.PutUint16(message[0:2], 0x1102)
	binary.BigEndian.PutUint16(message[2:4], controlHeaderSize+6)
	binary.BigEndian.PutUint16(message[4:6], stopFlowSequence)
	binary.BigEndian.PutUint16(message[6:8], stopFlowOpcode)
	copy(message[controlHeaderSize:], handle[:])
	return message
}

// exchangeControl sends request and returns the body of the first successful
// response that echoes its sequence number and opcode.
func exchangeControl(ctx context.Context, conn *net.UDPConn, request []byte, timeout time.Duration) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("send control request: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set control deadline: %w", err)
	}
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = conn.SetReadDeadline(time.Now())
	})
	defer stopInterrupt()

	buffer := make([]byte, 65535)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, errors.New("control response timed out")
			}
			return nil, fmt.Errorf("read control response: %w", err)
		}
		if n < controlHeaderSize {
			continue
		}
		length := int(binary.BigEndian.Uint16(buffer[2:4]))
		if length < controlHeaderSize || length > n || !bytes.Equal(buffer[4:8], request[4:8]) {
			continue
		}
		if status := binary.BigEndian.Uint16(buffer[8:10]); status != 0x0001 {
			opcode := binary.BigEndian.Uint16(request[6:8])
			return nil, fmt.Errorf("control request opcode 0x%04x failed with status 0x%04x", opcode, status)
		}
		return bytes.Clone(buffer[controlHeaderSize:length]), nil
	}
}
