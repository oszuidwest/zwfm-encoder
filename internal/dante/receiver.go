package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	sampleRate        = 48000
	outputChannels    = 2
	mediaHeaderLength = 9
	keepaliveInterval = 250 * time.Millisecond
	mediaTimeout      = 5 * time.Second
	readPollInterval  = 2 * time.Millisecond
	pcmBatchFrames    = sampleRate / 100
)

var keepalivePayload = []byte{0x13, 0x37}

// Receiver is a read-only S16LE stereo PCM source backed by a Dante flow.
type Receiver struct {
	reader    *io.PipeReader
	writer    *io.PipeWriter
	mediaConn *net.UDPConn
	localIP   net.IP
	remote    *net.UDPAddr
	dbcp1     uint16
	handle    flowHandle
	cancel    context.CancelFunc
	done      chan error
	closeOnce sync.Once
}

type mediaPacket struct {
	timestamp uint64
	frames    uint64
	pcm       []byte
}

type packetReorderer struct {
	expected    uint64
	started     bool
	missingFrom time.Time
	wait        time.Duration
	pending     map[uint64]mediaPacket
}

type pcmBatchWriter struct {
	destination io.Writer
	buffer      []byte
	targetSize  int
}

// Open resolves the configured transmitter channels and starts receiving them.
func Open(ctx context.Context, input string) (*Receiver, error) {
	config, err := ParseInput(input)
	if err != nil {
		return nil, err
	}

	channels, localIP, err := resolveChannels(ctx, &config)
	if err != nil {
		return nil, err
	}

	mediaConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
	if err != nil {
		return nil, fmt.Errorf("open Dante media socket: %w", err)
	}

	receiverName := config.Receiver
	if receiverName == "" {
		receiverName = defaultReceiverName()
	}
	bytesPerSample := channels[0].bitsPerSample / 8
	fppLimit := maxMediaPayload / (outputChannels * bytesPerSample)
	fpp := min(channels[0].fppMax, fppLimit)
	if fpp < channels[0].fppMin {
		_ = mediaConn.Close()
		return nil, fmt.Errorf(
			"dante transmitter requires at least %d frames per packet; MTU permits %d",
			channels[0].fppMin,
			fppLimit,
		)
	}

	request := flowRequest{
		localIP:       localIP,
		mediaPort:     uint16(mediaConn.LocalAddr().(*net.UDPAddr).Port), //nolint:gosec // UDP ports are uint16 values.
		receiverName:  receiverName,
		flowName:      "encoder_1",
		sampleRate:    sampleRate,
		bitsPerSample: uint32(channels[0].bitsPerSample), //nolint:gosec // validated as 16, 24, or 32.
		fpp:           uint16(fpp),                       //nolint:gosec // fpp is bounded by a 1400-byte payload.
		channelIDs:    []uint16{channels[0].ID, channels[1].ID},
	}
	handle, err := requestFlow(ctx, channels[0].address, channels[0].dbcp1, &request)
	if err != nil {
		_ = mediaConn.Close()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	receiver := &Receiver{
		reader:    reader,
		writer:    writer,
		mediaConn: mediaConn,
		localIP:   localIP,
		remote:    channels[0].address,
		dbcp1:     channels[0].dbcp1,
		handle:    handle,
		cancel:    cancel,
		done:      make(chan error, 1),
	}

	go receiver.run(runCtx, channels[0].bitsPerSample, config.ReorderWait)
	return receiver, nil
}

// Read reads interleaved 48 kHz stereo S16LE PCM.
func (r *Receiver) Read(buffer []byte) (int, error) {
	return r.reader.Read(buffer)
}

// Wait blocks until reception ends and returns its terminal error.
func (r *Receiver) Wait() error {
	return <-r.done
}

// Close stops network reception and unblocks outstanding reads and writes.
func (r *Receiver) Close() error {
	var closeErr error
	r.closeOnce.Do(func() {
		r.cancel()
		closeErr = errors.Join(r.mediaConn.Close(), r.reader.Close(), r.writer.Close())
	})
	return closeErr
}

func (r *Receiver) run(ctx context.Context, bitsPerSample int, reorderWait time.Duration) {
	terminalErr := r.receive(ctx, bitsPerSample, reorderWait)
	if errors.Is(terminalErr, context.Canceled) || errors.Is(terminalErr, net.ErrClosed) {
		terminalErr = nil
	}
	_ = r.writer.CloseWithError(terminalErr)
	_ = stopFlow(r.localIP, r.remote, r.dbcp1, r.handle)
	r.done <- terminalErr
}

func (r *Receiver) receive(ctx context.Context, bitsPerSample int, reorderWait time.Duration) error {
	buffer := make([]byte, 1500)
	pcmWriter := &pcmBatchWriter{
		destination: r.writer,
		buffer:      make([]byte, 0, pcmBatchFrames*outputChannels*2),
		targetSize:  pcmBatchFrames * outputChannels * 2,
	}
	defer func() {
		_ = pcmWriter.Flush()
	}()
	reorderer := packetReorderer{
		wait:    reorderWait,
		pending: make(map[uint64]mediaPacket),
	}

	var lastSource *net.UDPAddr
	lastPacket := time.Now()
	lastKeepalive := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.mediaConn.SetReadDeadline(time.Now().Add(readPollInterval)); err != nil {
			return fmt.Errorf("set Dante media deadline: %w", err)
		}

		size, source, err := r.mediaConn.ReadFromUDP(buffer)
		switch {
		case err == nil:
			packet, decodeErr := decodeMediaPacket(buffer[:size], bitsPerSample)
			if decodeErr != nil {
				return decodeErr
			}
			lastSource = source
			lastPacket = time.Now()
			if err := reorderer.add(pcmWriter, packet, lastPacket); err != nil {
				return err
			}
		case errors.Is(err, os.ErrDeadlineExceeded):
			if err := reorderer.flushGap(pcmWriter, time.Now()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("receive Dante media: %w", err)
		}

		now := time.Now()
		if lastSource != nil && now.Sub(lastKeepalive) >= keepaliveInterval {
			if _, err := r.mediaConn.WriteToUDP(keepalivePayload, lastSource); err != nil {
				return fmt.Errorf("send Dante keepalive: %w", err)
			}
			lastKeepalive = now
		}
		if now.Sub(lastPacket) >= mediaTimeout {
			return errors.New("dante media timed out")
		}
	}
}

func (w *pcmBatchWriter) Write(pcm []byte) (int, error) {
	written := len(pcm)
	for len(pcm) > 0 {
		remaining := w.targetSize - len(w.buffer)
		copySize := min(len(pcm), remaining)
		w.buffer = append(w.buffer, pcm[:copySize]...)
		pcm = pcm[copySize:]
		if len(w.buffer) != w.targetSize {
			continue
		}
		if err := w.Flush(); err != nil {
			return 0, err
		}
	}
	return written, nil
}

func (w *pcmBatchWriter) Flush() error {
	if len(w.buffer) == 0 {
		return nil
	}
	written, err := w.destination.Write(w.buffer)
	if err != nil {
		return err
	}
	if written != len(w.buffer) {
		return io.ErrShortWrite
	}
	w.buffer = w.buffer[:0]
	return nil
}

func decodeMediaPacket(packet []byte, bitsPerSample int) (mediaPacket, error) {
	if len(packet) < mediaHeaderLength {
		return mediaPacket{}, errors.New("dante media packet is shorter than its header")
	}
	bytesPerSample := bitsPerSample / 8
	frameSize := outputChannels * bytesPerSample
	payload := packet[mediaHeaderLength:]
	if bytesPerSample < 2 || bytesPerSample > 4 || len(payload)%frameSize != 0 {
		return mediaPacket{}, fmt.Errorf("invalid %d-bit Dante media payload length %d", bitsPerSample, len(payload))
	}

	frames := len(payload) / frameSize
	pcm := make([]byte, frames*outputChannels*2)
	for frame := range frames {
		for channel := range outputChannels {
			sourceOffset := frame*frameSize + channel*bytesPerSample
			destinationOffset := (frame*outputChannels + channel) * 2
			// Dante PCM is big-endian and left-aligned. Taking its two most
			// significant bytes performs the same S32-to-S16 conversion used
			// by Inferno while preserving the full 16-bit encoder input range.
			pcm[destinationOffset] = payload[sourceOffset+1]
			pcm[destinationOffset+1] = payload[sourceOffset]
		}
	}

	seconds := binary.BigEndian.Uint32(packet[1:5])
	subsecond := binary.BigEndian.Uint32(packet[5:9])
	return mediaPacket{
		timestamp: uint64(seconds)*sampleRate + uint64(subsecond),
		frames:    uint64(frames), //nolint:gosec // frames is derived from a non-negative slice length.
		pcm:       pcm,
	}, nil
}

func (r *packetReorderer) add(writer io.Writer, packet mediaPacket, now time.Time) error {
	if !r.started {
		r.started = true
		r.expected = packet.timestamp
	}
	if packet.timestamp < r.expected {
		return nil
	}
	r.pending[packet.timestamp] = packet
	return r.drain(writer, now)
}

func (r *packetReorderer) drain(writer io.Writer, now time.Time) error {
	for {
		packet, ok := r.pending[r.expected]
		if !ok {
			if len(r.pending) > 0 && r.missingFrom.IsZero() {
				r.missingFrom = now
			}
			return nil
		}
		if _, err := writer.Write(packet.pcm); err != nil {
			return fmt.Errorf("write Dante PCM: %w", err)
		}
		delete(r.pending, r.expected)
		r.expected += packet.frames
		r.missingFrom = time.Time{}
	}
}

func (r *packetReorderer) flushGap(writer io.Writer, now time.Time) error {
	if len(r.pending) == 0 || r.missingFrom.IsZero() || now.Sub(r.missingFrom) < r.wait {
		return nil
	}

	nextTimestamp := uint64(math.MaxUint64)
	for timestamp := range r.pending {
		if timestamp < nextTimestamp {
			nextTimestamp = timestamp
		}
	}
	if nextTimestamp <= r.expected {
		return r.drain(writer, now)
	}

	missingFrames := nextTimestamp - r.expected
	if missingFrames > sampleRate {
		return fmt.Errorf("dante media gap is too large: %d frames", missingFrames)
	}
	silence := make([]byte, int(missingFrames)*outputChannels*2)
	if _, err := writer.Write(silence); err != nil {
		return fmt.Errorf("write Dante gap silence: %w", err)
	}
	r.expected = nextTimestamp
	r.missingFrom = time.Time{}
	return r.drain(writer, now)
}

func defaultReceiverName() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "ZWFM Encoder"
	}
	hostname = strings.TrimSpace(hostname)
	if len(hostname) > 31 {
		hostname = hostname[:31]
	}
	return hostname
}
