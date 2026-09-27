package dante

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
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
	closeOnce   sync.Once
	terminal    error
	flowHandle  [6]byte
	bits        uint16
}

// Open discovers the requested channels, requests a stereo flow, and starts
// receiving it. The supplied context controls setup and the receiver lifetime.
func Open(ctx context.Context, input string) (*Receiver, error) {
	config, err := parseInput(input)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	interfaces, err := discoveryInterfaces(config.interfaceID)
	if err != nil {
		return nil, err
	}

	left, err := discoverChannel(
		ctx,
		interfaces,
		config.transmitter,
		config.left,
		nil,
	)
	if err != nil {
		return nil, err
	}
	right, err := discoverChannel(
		ctx,
		interfaces,
		config.transmitter,
		config.right,
		left.address,
	)
	if err != nil {
		return nil, err
	}
	if err := validateChannels(left, right); err != nil {
		return nil, err
	}
	localIP, err := selectLocalAddress(config.interfaceID, interfaces, left.address, left.port)
	if err != nil {
		return nil, err
	}

	var mediaConn *net.UDPConn
	var controlConn *net.UDPConn
	success := false
	defer func() {
		if success {
			return
		}
		if controlConn != nil {
			_ = controlConn.Close()
		}
		if mediaConn != nil {
			_ = mediaConn.Close()
		}
	}()

	mediaConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open media socket: %w", err)
	}
	mediaAddress := mediaConn.LocalAddr().(*net.UDPAddr)
	controlConn, err = net.DialUDP(
		"udp4",
		&net.UDPAddr{IP: localIP, Port: 0},
		&net.UDPAddr{IP: left.address, Port: int(left.port)},
	)
	if err != nil {
		return nil, fmt.Errorf("open control socket: %w", err)
	}

	fppMaximum := min(left.fppMax, right.fppMax)
	fppMinimum := max(left.fppMin, right.fppMin)
	fpp, err := chooseFPP(fppMaximum, fppMinimum, left.bits/8)
	if err != nil {
		return nil, err
	}
	request, err := buildFlowRequest(&flowParameters{
		bits:       left.bits,
		channelIDs: [mediaChannels]uint16{left.id, right.id},
		fpp:        fpp,
		receiver:   receiverName(),
		mediaPort:  mediaAddress.AddrPort().Port(),
		localIP:    localIP,
	})
	if err != nil {
		return nil, err
	}
	body, err := exchangeControl(
		ctx,
		controlConn,
		request,
		requestFlowSequence,
		requestFlowOpcode,
		3*time.Second,
	)
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
		bits:        left.bits,
	}
	copy(receiver.flowHandle[:], body[:6])
	success = true
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

func selectLocalAddress(
	interfaceID string,
	interfaces []net.Interface,
	remoteIP net.IP,
	remotePort uint16,
) (net.IP, error) {
	if interfaceID != "" {
		return firstIPv4Address(&interfaces[0])
	}
	conn, err := net.DialUDP(
		"udp4",
		nil,
		&net.UDPAddr{IP: remoteIP, Port: int(remotePort)},
	)
	if err != nil {
		return nil, fmt.Errorf("select local address for transmitter: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	address := conn.LocalAddr().(*net.UDPAddr)
	if address.IP.To4() == nil {
		return nil, errors.New("operating system selected no local IPv4 address")
	}
	return append(net.IP(nil), address.IP.To4()...), nil
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
	if err != nil {
		_ = r.writer.CloseWithError(err)
	} else {
		_ = r.writer.Close()
	}

	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	request := buildStopRequest(r.flowHandle)
	_, _ = exchangeControl(
		stopCtx,
		r.controlConn,
		request,
		stopFlowSequence,
		stopFlowOpcode,
		time.Second,
	)
	stopCancel()
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
	r.closeOnce.Do(r.cancel)
	<-r.done
	return nil
}
