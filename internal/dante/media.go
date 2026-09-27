package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
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
		return false, p.advance(now)
	}
	if _, duplicate := p.pending[packet.position]; duplicate {
		return false, p.advance(now)
	}
	p.pending[packet.position] = packet
	return true, p.advance(now)
}

func (p *mediaProcessor) parse(datagram []byte) (mediaPacket, bool) {
	if len(datagram) <= mediaHeaderSize || (len(datagram)-mediaHeaderSize)%p.frameSize() != 0 {
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

	maps.DeleteFunc(p.pending, func(position uint64, _ mediaPacket) bool {
		return position < p.expected
	})
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

func (p *mediaProcessor) frameSize() int {
	return mediaChannels * p.bytesPerSample
}

func (p *mediaProcessor) packetFrames(packet mediaPacket) uint64 {
	return uint64(len(packet.payload) / p.frameSize()) //nolint:gosec // Lengths are non-negative.
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

// appendPacket converts the most significant sample bits to S16LE.
func (p *mediaProcessor) appendPacket(packet mediaPacket) error {
	frameSize := p.frameSize()
	output := make([]byte, 0, len(packet.payload)/frameSize*outputFrameSize)
	for offset := 0; offset < len(packet.payload); offset += frameSize {
		for channel := range mediaChannels {
			sample := offset + channel*p.bytesPerSample
			output = append(output, packet.payload[sample+1], packet.payload[sample])
		}
	}
	_, err := p.output.Write(output)
	return err
}

func (p *mediaProcessor) appendSilence(frames uint64) error {
	var zeros [outputBatchSize]byte
	for frames > 0 {
		batch := min(frames, outputBatchSize/outputFrameSize)
		if _, err := p.output.Write(zeros[:batch*outputFrameSize]); err != nil {
			return err
		}
		frames -= batch
	}
	return nil
}

// receiveMedia requires conn to be closed when ctx is cancelled.
func receiveMedia(
	ctx context.Context,
	conn *net.UDPConn,
	transmitter net.IP,
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
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set media deadline: %w", err)
		}
		n, source, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return fmt.Errorf("receive media: %w", err)
		}
		if !source.IP.Equal(transmitter) {
			continue
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
