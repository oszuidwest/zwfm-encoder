package dante

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestReceiverCloseSemantics(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiver, stopReceived, cleanup := newTestReceiver(t, ctx, cancel)
	defer cleanup()

	var waitGroup sync.WaitGroup
	for range 8 {
		waitGroup.Go(func() {
			if err := receiver.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	waitGroup.Wait()

	readDone := make(chan error, 1)
	go func() {
		_, err := receiver.Read(make([]byte, 4))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Read error = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock")
	}
	if err := waitForReceiver(t, receiver); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	select {
	case <-stopReceived:
	case <-time.After(time.Second):
		t.Fatal("stop-flow request was not received")
	}
}

func TestReceiverContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	receiver, stopReceived, cleanup := newTestReceiver(t, ctx, cancel)
	defer cleanup()
	cancel()
	if err := waitForReceiver(t, receiver); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := receiver.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want EOF", err)
	}
	select {
	case <-stopReceived:
	case <-time.After(time.Second):
		t.Fatal("stop-flow request was not received")
	}
}

func newTestReceiver(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
) (receiver *Receiver, stopReceived <-chan struct{}, cleanup func()) {
	t.Helper()
	mediaConn := listenLoopbackUDP(t)
	controlServer := listenLoopbackUDP(t)
	controlConn, err := net.DialUDP("udp4", nil, controlServer.LocalAddr().(*net.UDPAddr))
	if err != nil {
		_ = mediaConn.Close()
		_ = controlServer.Close()
		t.Fatal(err)
	}

	stopChannel := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		buffer := make([]byte, 64)
		n, peer, readErr := controlServer.ReadFromUDP(buffer)
		if readErr != nil || n < controlHeaderSize {
			return
		}
		sequence := binary.BigEndian.Uint16(buffer[4:6])
		opcode := binary.BigEndian.Uint16(buffer[6:8])
		if opcode == stopFlowOpcode {
			close(stopChannel)
		}
		_, _ = controlServer.WriteToUDP(controlResponse(sequence, opcode, 1, nil), peer)
	}()

	reader, writer := io.Pipe()
	receiver = &Receiver{
		reader:      reader,
		writer:      writer,
		mediaConn:   mediaConn,
		controlConn: controlConn,
		cancel:      cancel,
		done:        make(chan struct{}),
		flowHandle:  [6]byte{1, 2, 3, 4, 5, 6},
		bits:        16,
	}
	go receiver.run(ctx)
	cleanup = func() {
		_ = receiver.Close()
		select {
		case <-receiver.done:
		case <-time.After(2 * time.Second):
		}
		_ = controlServer.Close()
		<-serverDone
	}
	return receiver, stopChannel, cleanup
}

func waitForReceiver(t *testing.T, receiver *Receiver) error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- receiver.Wait()
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return")
		return nil
	}
}
