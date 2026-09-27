package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	requestHeaderLength = 10
	flowStartCode       = 0x1102
	requestFlowOpcode   = 0x0100
	stopFlowOpcode      = 0x0101
	responseOK          = 1
	controlTimeout      = 3 * time.Second
	maxMediaPayload     = 1400
)

type flowHandle [6]byte

type flowRequest struct {
	localIP       net.IP
	mediaPort     uint16
	receiverName  string
	flowName      string
	sampleRate    uint32
	bitsPerSample uint32
	fpp           uint16
	channelIDs    []uint16
}

type controlExchange struct {
	localIP   net.IP
	remote    *net.UDPAddr
	startCode uint16
	sequence  uint16
	opcode    uint16
	content   []byte
}

func requestFlow(
	ctx context.Context,
	remote *net.UDPAddr,
	dbcp1 uint16,
	request *flowRequest,
) (flowHandle, error) {
	body, err := makeFlowRequestBody(request)
	if err != nil {
		return flowHandle{}, err
	}
	response, err := exchangeControlPacket(ctx, controlExchange{
		localIP:   request.localIP,
		remote:    remote,
		startCode: dbcp1,
		sequence:  1,
		opcode:    requestFlowOpcode,
		content:   body,
	})
	if err != nil {
		return flowHandle{}, fmt.Errorf("request Dante flow: %w", err)
	}
	if len(response) < len(flowHandle{}) {
		return flowHandle{}, errors.New("request Dante flow: response has no flow handle")
	}
	var handle flowHandle
	copy(handle[:], response[:len(handle)])
	return handle, nil
}

func stopFlow(localIP net.IP, remote *net.UDPAddr, dbcp1 uint16, handle flowHandle) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := exchangeControlPacket(ctx, controlExchange{
		localIP:   localIP,
		remote:    remote,
		startCode: dbcp1,
		sequence:  2,
		opcode:    stopFlowOpcode,
		content:   handle[:],
	})
	return err
}

func makeFlowRequestBody(request *flowRequest) ([]byte, error) {
	if request.localIP.To4() == nil {
		return nil, errors.New("dante flow request requires an IPv4 address")
	}
	if len(request.channelIDs) == 0 {
		return nil, errors.New("dante flow request requires at least one channel")
	}

	stringsOffset := 0x26 + len(request.channelIDs)*2 + requestHeaderLength
	if stringsOffset > int(^uint16(0)) || len(request.channelIDs) > int(^uint16(0)) {
		return nil, errors.New("dante flow request has too many channels")
	}
	body := make([]byte, stringsOffset-requestHeaderLength)
	strings := make([]byte, 0, len(request.receiverName)+len(request.flowName)+16)
	strings = append(strings, request.receiverName...)
	strings = append(strings, 0)
	flowNameOffset := stringsOffset + len(strings)
	strings = append(strings, request.flowName...)
	strings = append(strings, 0)

	for (len(strings)+stringsOffset)%8 != 0 {
		strings = append(strings, 0)
	}
	addressOffset := stringsOffset + len(strings)
	strings = binary.BigEndian.AppendUint16(strings, 0x0802)
	strings = binary.BigEndian.AppendUint16(strings, request.mediaPort)
	strings = append(strings, request.localIP.To4()...)
	if requestHeaderLength+len(body)+len(strings) > int(^uint16(0)) {
		return nil, errors.New("dante flow request exceeds the protocol packet size")
	}

	stringsOffsetValue, err := checkedUint16(stringsOffset, "strings offset")
	if err != nil {
		return nil, err
	}
	channelCount, err := checkedUint16(len(request.channelIDs), "channel count")
	if err != nil {
		return nil, err
	}
	addressOffsetValue, err := checkedUint16(addressOffset, "address offset")
	if err != nil {
		return nil, err
	}
	channelDescriptorOffset, err := checkedUint16(0x1c+2*len(request.channelIDs), "channel descriptor offset")
	if err != nil {
		return nil, err
	}
	flowNameOffsetValue, err := checkedUint16(flowNameOffset, "flow name offset")
	if err != nil {
		return nil, err
	}

	binary.BigEndian.PutUint16(body[0:2], stringsOffsetValue)
	binary.BigEndian.PutUint32(body[2:6], request.sampleRate)
	binary.BigEndian.PutUint32(body[6:10], request.bitsPerSample)
	binary.BigEndian.PutUint16(body[10:12], 1)
	binary.BigEndian.PutUint16(body[12:14], channelCount)
	binary.BigEndian.PutUint16(body[14:16], addressOffsetValue)
	offset := 16
	for _, channelID := range request.channelIDs {
		binary.BigEndian.PutUint16(body[offset:offset+2], channelID)
		offset += 2
	}
	binary.BigEndian.PutUint16(body[offset:offset+2], channelDescriptorOffset)
	offset += 2
	binary.BigEndian.PutUint16(body[offset:offset+2], 0x0a00)
	offset += 2
	binary.BigEndian.PutUint16(body[offset:offset+2], 0x0002)
	offset += 2
	binary.BigEndian.PutUint16(body[offset:offset+2], request.fpp)
	offset += 2
	binary.BigEndian.PutUint16(body[offset:offset+2], flowNameOffsetValue)
	// The final twelve bytes are an opaque receiver identifier in the native
	// protocol. Inferno uses zeroes, which is accepted by tested transmitters.

	return append(body, strings...), nil
}

func checkedUint16(value int, field string) (uint16, error) {
	if value < 0 || value > int(^uint16(0)) {
		return 0, fmt.Errorf("dante %s %d does not fit in uint16", field, value)
	}
	return uint16(value), nil //nolint:gosec // Bounds are checked immediately above.
}

func exchangeControlPacket(ctx context.Context, exchange controlExchange) ([]byte, error) {
	localAddress := &net.UDPAddr{IP: exchange.localIP}
	conn, err := net.DialUDP("udp4", localAddress, exchange.remote)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close()
	}()
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancellation()

	deadline := time.Now().Add(controlTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	packet := makeControlPacket(exchange.startCode, exchange.sequence, exchange.opcode, 0, exchange.content)
	if _, err := conn.Write(packet); err != nil {
		return nil, err
	}

	buffer := make([]byte, 1500)
	for {
		size, readErr := conn.Read(buffer)
		if readErr != nil {
			if errors.Is(readErr, os.ErrDeadlineExceeded) && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, readErr
		}
		if size < requestHeaderLength {
			continue
		}
		responseStartCode := binary.BigEndian.Uint16(buffer[0:2])
		responseLength := int(binary.BigEndian.Uint16(buffer[2:4]))
		responseSequence := binary.BigEndian.Uint16(buffer[4:6])
		responseOpcode := binary.BigEndian.Uint16(buffer[6:8])
		wrongResponse := responseStartCode != exchange.startCode ||
			responseSequence != exchange.sequence ||
			responseOpcode != exchange.opcode
		if wrongResponse || responseLength < requestHeaderLength || responseLength > size {
			continue
		}
		code := binary.BigEndian.Uint16(buffer[8:10])
		if code != responseOK {
			return nil, fmt.Errorf("transmitter returned Dante error 0x%04x", code)
		}
		return append([]byte(nil), buffer[requestHeaderLength:responseLength]...), nil
	}
}

func makeControlPacket(startCode, sequence, opcode, code uint16, content []byte) []byte {
	packet := make([]byte, requestHeaderLength+len(content))
	binary.BigEndian.PutUint16(packet[0:2], startCode)
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet))) //nolint:gosec // Control messages are bounded to 1500 bytes.
	binary.BigEndian.PutUint16(packet[4:6], sequence)
	binary.BigEndian.PutUint16(packet[6:8], opcode)
	binary.BigEndian.PutUint16(packet[8:10], code)
	copy(packet[requestHeaderLength:], content)
	return packet
}
