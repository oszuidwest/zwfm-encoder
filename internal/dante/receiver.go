package dante

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Receiver is a receive-only stereo PCM stream.
type Receiver struct {
	reader      *io.PipeReader
	writer      *io.PipeWriter
	mediaConn   *net.UDPConn
	controlConn *net.UDPConn
	cancel      context.CancelFunc
	done        chan struct{}
	terminal    error
	flowHandle  [6]byte
	bits        uint16
}

// Open discovers the requested channels, requests a stereo flow, and starts
// receiving it. The supplied context controls setup and the receiver lifetime.
func Open(ctx context.Context, input string) (_ *Receiver, err error) {
	config, err := parseInput(input)
	if err != nil {
		return nil, err
	}
	interfaces, err := discoveryInterfaces(config.interfaceID)
	if err != nil {
		return nil, err
	}
	left, err := discoverChannel(ctx, interfaces, config.transmitter, config.left, nil)
	if err != nil {
		return nil, err
	}
	right, err := discoverChannel(ctx, interfaces, config.transmitter, config.right, left.address)
	if err != nil {
		return nil, err
	}
	if err := validateChannels(left, right); err != nil {
		return nil, err
	}
	fpp, err := chooseFPP(min(left.fppMax, right.fppMax), max(left.fppMin, right.fppMin), left.bits/8)
	if err != nil {
		return nil, err
	}
	transmitter := &net.UDPAddr{IP: left.address, Port: int(left.port)}
	localIP, err := selectLocalAddress(config.interfaceID, interfaces, transmitter)
	if err != nil {
		return nil, err
	}

	mediaConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
	if err != nil {
		return nil, fmt.Errorf("open media socket: %w", err)
	}
	controlConn, err := net.DialUDP("udp4", &net.UDPAddr{IP: localIP}, transmitter)
	if err != nil {
		_ = mediaConn.Close()
		return nil, fmt.Errorf("open control socket: %w", err)
	}
	defer func() {
		if err != nil {
			_ = controlConn.Close()
			_ = mediaConn.Close()
		}
	}()

	request := buildFlowRequest(&flowParameters{
		bits:       left.bits,
		channelIDs: [mediaChannels]uint16{left.id, right.id},
		fpp:        fpp,
		receiver:   receiverName(),
		mediaPort:  mediaConn.LocalAddr().(*net.UDPAddr).AddrPort().Port(),
		localIP:    localIP,
	})
	body, err := exchangeControl(ctx, controlConn, request, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("request flow: %w", err)
	}
	if len(body) < 6 {
		return nil, fmt.Errorf("request flow response body is %d bytes; need 6", len(body))
	}

	reader, writer := io.Pipe()
	receiveCtx, cancelReceive := context.WithCancel(ctx)
	receiver := &Receiver{
		reader:      reader,
		writer:      writer,
		mediaConn:   mediaConn,
		controlConn: controlConn,
		cancel:      cancelReceive,
		done:        make(chan struct{}),
		flowHandle:  [6]byte(body),
		bits:        left.bits,
	}
	go receiver.run(receiveCtx)
	return receiver, nil
}

func validateChannels(left, right channelInfo) error {
	if !left.address.Equal(right.address) || left.port != right.port {
		return errors.New("channels are advertised by different transmitter addresses or ports")
	}
	if left.bits != right.bits || left.rate != right.rate || left.nchan != right.nchan {
		return errors.New("channels have inconsistent encoding parameters")
	}
	if left.rate != sampleRate {
		return fmt.Errorf("unsupported sample rate %d; need 48000", left.rate)
	}
	if left.nchan < mediaChannels {
		return fmt.Errorf("transmitter supports only %d channel per flow; need 2", left.nchan)
	}
	if left.bits != 16 && left.bits != 24 && left.bits != 32 {
		return fmt.Errorf("unsupported bits per sample %d", left.bits)
	}
	return nil
}

func selectLocalAddress(interfaceID string, interfaces []net.Interface, transmitter *net.UDPAddr) (net.IP, error) {
	if interfaceID != "" {
		return firstIPv4Address(&interfaces[0])
	}
	conn, err := net.DialUDP("udp4", nil, transmitter)
	if err != nil {
		return nil, fmt.Errorf("select local address for transmitter: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	address := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	if address == nil {
		return nil, errors.New("operating system selected no local IPv4 address")
	}
	return address, nil
}

func (r *Receiver) run(ctx context.Context) {
	stopInterrupt := context.AfterFunc(ctx, r.interrupt)
	bufferedOutput := bufio.NewWriterSize(r.writer, outputBatchSize)
	err := receiveMedia(ctx, r.mediaConn, r.bits, bufferedOutput)
	if ctx.Err() != nil {
		err = nil
	} else {
		_ = bufferedOutput.Flush()
	}
	stopInterrupt()
	_ = r.mediaConn.Close()
	_ = r.writer.CloseWithError(err)

	_, _ = exchangeControl(context.WithoutCancel(ctx), r.controlConn, buildStopRequest(r.flowHandle), time.Second)
	_ = r.controlConn.Close()

	r.terminal = err
	close(r.done)
	r.cancel()
}

func (r *Receiver) interrupt() {
	_ = r.mediaConn.Close()
	_ = r.writer.Close()
}

// Read reads interleaved signed 16-bit little-endian stereo PCM samples.
func (r *Receiver) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

// Wait blocks until reception and flow shutdown have completed.
func (r *Receiver) Wait() error {
	<-r.done
	return r.terminal
}

// Close stops reception. It is safe to call repeatedly and concurrently.
func (r *Receiver) Close() error {
	r.cancel()
	<-r.done
	return nil
}
