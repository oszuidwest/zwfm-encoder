package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	sampleRate         = 48000
	mediaChannels      = 2
	mediaHeaderSize    = 9
	outputFrameSize    = 4
	outputBatchSize    = 1920
	gapWait            = 4 * time.Millisecond
	mediaTimeout       = 5 * time.Second
	keepaliveInterval  = 200 * time.Millisecond
	maximumGapInFrames = 48000
)

type mediaPacket struct {
	position uint64
	payload  []byte
}

type mediaProcessor struct {
	bytesPerSample int
	output         io.Writer
	initialized    bool
	expected       uint64
	pending        map[uint64]mediaPacket
	gapStarted     time.Time
}

func newMediaProcessor(bits uint16, output io.Writer) *mediaProcessor {
	return &mediaProcessor{
		bytesPerSample: int(bits / 8),
		output:         output,
		pending:        make(map[uint64]mediaPacket),
	}
}

func (p *mediaProcessor) receive(datagram []byte, now time.Time) (bool, error) {
	packet, ok := p.parse(datagram)
	if !ok {
		return false, nil
	}
	if !p.initialized {
		p.initialized = true
		p.expected = packet.position
	}
	if packet.position < p.expected {
		return true, p.advance(now)
	}
	if _, duplicate := p.pending[packet.position]; !duplicate {
		p.pending[packet.position] = packet
	}
	return true, p.advance(now)
}

func (p *mediaProcessor) parse(datagram []byte) (mediaPacket, bool) {
	if len(datagram) < mediaHeaderSize {
		return mediaPacket{}, false
	}
	frameSize := mediaChannels * p.bytesPerSample
	if frameSize <= 0 || (len(datagram)-mediaHeaderSize)%frameSize != 0 {
		return mediaPacket{}, false
	}
	if len(datagram) == mediaHeaderSize {
		return mediaPacket{}, false
	}
	seconds := uint64(binary.BigEndian.Uint32(datagram[1:5]))
	offset := uint64(binary.BigEndian.Uint32(datagram[5:9]))
	return mediaPacket{
		position: seconds*sampleRate + offset,
		payload:  append([]byte(nil), datagram[mediaHeaderSize:]...),
	}, true
}

func (p *mediaProcessor) advance(now time.Time) error {
	for {
		packet, ok := p.pending[p.expected]
		if !ok {
			break
		}
		delete(p.pending, p.expected)
		if err := p.appendPacket(packet); err != nil {
			return err
		}
		p.expected += p.packetFrames(packet)
	}

	for position := range p.pending {
		if position < p.expected {
			delete(p.pending, position)
		}
	}
	minimum, ok := p.earliestPending()
	if !ok {
		p.gapStarted = time.Time{}
		return nil
	}
	if p.gapStarted.IsZero() {
		p.gapStarted = now
		return nil
	}
	if now.Sub(p.gapStarted) < gapWait {
		return nil
	}

	missing := minimum - p.expected
	if missing > maximumGapInFrames {
		return fmt.Errorf("media gap of %d frames exceeds one second", missing)
	}
	if err := p.appendSilence(missing); err != nil {
		return err
	}
	p.expected = minimum
	return p.advance(now)
}

func (p *mediaProcessor) packetFrames(packet mediaPacket) uint64 {
	frameSize := mediaChannels * p.bytesPerSample
	var frames uint64
	for offset := 0; offset < len(packet.payload); offset += frameSize {
		frames++
	}
	return frames
}

func (p *mediaProcessor) earliestPending() (uint64, bool) {
	var minimum uint64
	found := false
	for position := range p.pending {
		if !found || position < minimum {
			minimum = position
			found = true
		}
	}
	return minimum, found
}

func (p *mediaProcessor) appendPacket(packet mediaPacket) error {
	frameSize := mediaChannels * p.bytesPerSample
	frames := len(packet.payload) / frameSize
	output := make([]byte, frames*outputFrameSize)
	outputOffset := 0
	for offset := 0; offset < len(packet.payload); offset += frameSize {
		for channel := range mediaChannels {
			sample := offset + channel*p.bytesPerSample
			output[outputOffset] = packet.payload[sample+1]
			output[outputOffset+1] = packet.payload[sample]
			outputOffset += 2
		}
	}
	return writeOutput(p.output, output)
}

func (p *mediaProcessor) appendSilence(frames uint64) error {
	var zeroBatch [outputBatchSize]byte
	const framesPerBatch = outputBatchSize / outputFrameSize
	for frames >= framesPerBatch {
		if err := writeOutput(p.output, zeroBatch[:]); err != nil {
			return err
		}
		frames -= framesPerBatch
	}
	var zeroFrame [outputFrameSize]byte
	for range frames {
		if err := writeOutput(p.output, zeroFrame[:]); err != nil {
			return err
		}
	}
	return nil
}

func writeOutput(output io.Writer, data []byte) error {
	n, err := output.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func receiveMedia(
	ctx context.Context,
	conn *net.UDPConn,
	bits uint16,
	output io.Writer,
) error {
	processor := newMediaProcessor(bits, output)
	buffer := make([]byte, 65535)
	lastValid := time.Now()
	var keepaliveTarget *net.UDPAddr
	var nextKeepalive time.Time

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		if now.Sub(lastValid) >= mediaTimeout {
			return errors.New("media reception timed out after 5 seconds")
		}
		if err := processor.advance(now); err != nil {
			return err
		}
		if keepaliveTarget != nil && !now.Before(nextKeepalive) {
			_, _ = conn.WriteToUDP([]byte{0x13, 0x37}, keepaliveTarget)
			nextKeepalive = now.Add(keepaliveInterval)
		}

		deadline := lastValid.Add(mediaTimeout)
		if !processor.gapStarted.IsZero() {
			deadline = earlierTime(deadline, processor.gapStarted.Add(gapWait))
		}
		if keepaliveTarget != nil {
			deadline = earlierTime(deadline, nextKeepalive)
		}
		if contextDeadline, ok := ctx.Deadline(); ok {
			deadline = earlierTime(deadline, contextDeadline)
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set media deadline: %w", err)
		}
		n, source, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				continue
			}
			return fmt.Errorf("receive media: %w", err)
		}
		valid, err := processor.receive(buffer[:n], time.Now())
		if err != nil {
			return err
		}
		if valid {
			lastValid = time.Now()
			keepaliveTarget = source
			if nextKeepalive.IsZero() {
				nextKeepalive = time.Now().Add(keepaliveInterval)
			}
		}
	}
}

func earlierTime(left, right time.Time) time.Time {
	if right.Before(left) {
		return right
	}
	return left
}
