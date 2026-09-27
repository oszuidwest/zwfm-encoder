package dante

import (
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

func receiverName() string {
	name, err := os.Hostname()
	if err != nil {
		return "ZWFM Encoder"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "ZWFM Encoder"
	}
	result := make([]byte, 0, min(len(name), 31))
	for i := 0; i < len(name) && len(result) < 31; i++ {
		if name[i] >= 0x20 && name[i] <= 0x7e {
			result = append(result, name[i])
		}
	}
	if len(result) == 0 {
		return "ZWFM Encoder"
	}
	return string(result)
}

func buildFlowRequest(parameters *flowParameters) ([]byte, error) {
	localIP := parameters.localIP.To4()
	if localIP == nil {
		return nil, errors.New("flow request requires a local IPv4 address")
	}
	name := []byte(parameters.receiver)
	if len(name) > 31 {
		return nil, fmt.Errorf("receiver name is %d bytes; maximum is 31", len(name))
	}

	const stringOffset uint16 = controlHeaderSize + 38 + 2*mediaChannels
	flowName := []byte("encoder_1")
	flowNameOffset := stringOffset + 1
	for range name {
		flowNameOffset++
	}
	const flowNameLength uint16 = 9
	addressOffset := flowNameOffset + flowNameLength + 1
	if remainder := addressOffset % 8; remainder != 0 {
		addressOffset += 8 - remainder
	}
	totalLength := addressOffset + 8

	message := make([]byte, int(totalLength))
	binary.BigEndian.PutUint16(message[0:2], 0x1102)
	binary.BigEndian.PutUint16(message[2:4], totalLength)
	binary.BigEndian.PutUint16(message[4:6], requestFlowSequence)
	binary.BigEndian.PutUint16(message[6:8], requestFlowOpcode)

	body := message[controlHeaderSize:int(stringOffset)]
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

	copy(message[int(stringOffset):], name)
	copy(message[int(flowNameOffset):], flowName)
	binary.BigEndian.PutUint16(message[int(addressOffset):int(addressOffset)+2], 0x0802)
	binary.BigEndian.PutUint16(message[int(addressOffset)+2:int(addressOffset)+4], parameters.mediaPort)
	copy(message[int(addressOffset)+4:int(addressOffset)+8], localIP)
	return message, nil
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

func exchangeControl(
	ctx context.Context,
	conn *net.UDPConn,
	request []byte,
	sequence uint16,
	opcode uint16,
	timeout time.Duration,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("send control request: %w", err)
	}
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok {
		deadline = earlierTime(deadline, contextDeadline)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
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
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				return nil, errors.New("control response timed out")
			}
			return nil, fmt.Errorf("read control response: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n < controlHeaderSize {
			continue
		}
		length := int(binary.BigEndian.Uint16(buffer[2:4]))
		if length < controlHeaderSize || length > n {
			continue
		}
		if binary.BigEndian.Uint16(buffer[4:6]) != sequence ||
			binary.BigEndian.Uint16(buffer[6:8]) != opcode {
			continue
		}
		status := binary.BigEndian.Uint16(buffer[8:10])
		if status != 0x0001 {
			return nil, fmt.Errorf("control request opcode 0x%04x failed with status 0x%04x", opcode, status)
		}
		return append([]byte(nil), buffer[controlHeaderSize:length]...), nil
	}
}
