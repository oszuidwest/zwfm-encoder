package dante

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const testInput = "dante://tx/Left/Right"

func TestOpenEndToEnd(t *testing.T) {
	for _, bits := range []uint16{16, 24, 32} {
		t.Run(strconv.Itoa(int(bits)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.channels[0].bits, h.channels[1].bits = bits, bits
				h.start(t)
				r, err := Open(t.Context(), testInput)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Close() }()
				flow := <-h.flows
				checkUint16(t, flow.request, 4, 1, "flow request sequence")
				if rate := binary.BigEndian.Uint32(flow.request[controlHeaderSize+2:]); rate != 48000 {
					t.Fatalf("requested rate = %d, want 48000", rate)
				}
				checkUint16(t, flow.request, controlHeaderSize+16, 1, "left channel ID")
				checkUint16(t, flow.request, controlHeaderSize+18, 2, "right channel ID")
				checkUint16(t, flow.request, controlHeaderSize+26, 32, "chosen FPP")
				wire := map[uint16][]byte{
					16: {0x12, 0x34, 0xab, 0xcd},
					24: {0x12, 0x34, 0x56, 0xab, 0xcd, 0xef},
					32: {0x12, 0x34, 0x56, 0x78, 0xab, 0xcd, 0xef, 0x01},
				}[bits]
				for packet := uint32(0); packet < 16; packet++ {
					if _, err := h.media.WriteTo(mediaDatagram(packet*32, bytes.Repeat(wire, 32)), flow.target); err != nil {
						t.Fatal(err)
					}
				}
				got := make([]byte, outputBatchSize)
				if _, err := io.ReadFull(r, got); err != nil {
					t.Fatal(err)
				}
				if want := bytes.Repeat([]byte{0x34, 0x12, 0xcd, 0xab}, outputBatchSize/4); !bytes.Equal(got, want) {
					t.Fatal("Open did not deliver correctly oriented signed s16le PCM")
				}
				assertKeepalives(t, h, flow.target)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if err := r.Wait(); err != nil {
					t.Fatal(err)
				}
				assertStoppedFlow(t, h)
			})
		})
	}
}

func assertKeepalives(t *testing.T, h *fakeTransmitter, mediaTarget *net.UDPAddr) {
	t.Helper()
	previous := time.Now()
	for range 2 {
		buffer := make([]byte, 16)
		n, from, err := h.media.ReadFrom(buffer)
		if err != nil || !bytes.Equal(buffer[:n], []byte{0x13, 0x37}) || from.String() != mediaTarget.String() {
			t.Fatalf("keepalive = %x from %v, %v", buffer[:n], from, err)
		}
		if elapsed := time.Since(previous); elapsed != 200*time.Millisecond {
			t.Fatalf("keepalive interval = %s, want 200ms", elapsed)
		}
		previous = time.Now()
	}
}

func assertStoppedFlow(t *testing.T, h *fakeTransmitter) {
	t.Helper()
	select {
	case request := <-h.stops:
		if !bytes.Equal(request[controlHeaderSize:], []byte{1, 2, 3, 4, 5, 6}) {
			t.Fatalf("stop-flow handle = %x", request[controlHeaderSize:])
		}
	default:
		t.Fatal("Close did not send stop-flow")
	}
	if len(h.stops) != 0 {
		t.Fatal("stop-flow was sent more than once")
	}
}

func TestOpenCloseUnblocksAlreadyPendingRead(t *testing.T) {
	for _, cancelLifetime := range []bool{false, true} {
		name := "Close"
		if cancelLifetime {
			name = "context cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.start(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r, err := Open(ctx, testInput+"?interface=test0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Close() }()
				readDone := make(chan error, 1)
				go func() { _, err := r.Read(make([]byte, 4)); readDone <- err }()
				synctest.Wait() // Read is durably blocked on the actual PCM pipe before Close.
				select {
				case err := <-readDone:
					t.Fatalf("Read returned before Close: %v", err)
				default:
				}
				if cancelLifetime {
					cancel()
				} else if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if err := r.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := <-readDone; !errors.Is(err, io.EOF) {
					t.Fatalf("Read after Close = %v, want EOF", err)
				}
				assertStoppedFlow(t, h)
			})
		})
	}
}

func TestOpenMediaTimeoutStopsFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeTransmitter()
		defer h.close(t)
		h.start(t)
		r, err := Open(t.Context(), testInput)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		started := time.Now()
		_, readErr := r.Read(make([]byte, 4))
		if readErr == nil || !strings.Contains(readErr.Error(), "media reception timed out") || time.Since(started) != 5*time.Second {
			t.Fatalf("Read = %v after %s, want 5-second media timeout", readErr, time.Since(started))
		}
		if err := r.Wait(); err == nil || err.Error() != readErr.Error() {
			t.Fatalf("Wait = %v, want Read's terminal error", err)
		}
		assertStoppedFlow(t, h)
	})
}

func TestOpenCancellationDuringSetup(t *testing.T) {
	for _, stage := range []string{"Left", "Right", "control"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				if stage == "control" {
					h.dropFlow = true
				} else {
					h.dropChannel = stage
				}
				h.start(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					r, err := Open(ctx, testInput)
					if r != nil {
						_ = r.Close()
					}
					done <- err
				}()
				if stage == "control" {
					<-h.flows
				} else {
					for instance := range h.queries {
						if strings.HasPrefix(instance, stage+"@") {
							break
						}
					}
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel during %s = %v", stage, err)
				}
			})
		})
	}
}

func TestOpenCleansPartialSetupFailures(t *testing.T) {
	for _, tc := range []struct {
		name, want, input string
		configure         func(*fakeTransmitter)
	}{
		{name: "invalid URI", want: "path must contain exactly two", input: "dante://tx/Left", configure: func(*fakeTransmitter) {}},
		{name: "unknown interface", want: "unknown test interface", input: testInput + "?interface=missing", configure: func(*fakeTransmitter) {}},
		{name: "no usable interface", want: "no up, multicast", configure: func(h *fakeTransmitter) { h.interfaces = nil }},
		{name: "left discovery socket", want: "open discovery socket", configure: func(h *fakeTransmitter) { h.failListenAt = 1 }},
		{name: "right discovery socket", want: "open discovery socket", configure: func(h *fakeTransmitter) { h.failListenAt = 2 }},
		{name: "left discovery timeout", want: "context deadline exceeded", configure: func(h *fakeTransmitter) { h.dropChannel = "Left" }},
		{name: "right malformed discovery", want: "malformed response", configure: func(h *fakeTransmitter) { h.malformedChannel = "Right" }},
		{name: "route selection", want: "select local address", configure: func(h *fakeTransmitter) { h.failDialAt = 1 }},
		{name: "media socket", want: "open media socket", configure: func(h *fakeTransmitter) { h.failListenAt = 3 }},
		{name: "control socket", want: "open control socket", configure: func(h *fakeTransmitter) { h.failDialAt = 2 }},
		{name: "control timeout", want: "control response timed out", configure: func(h *fakeTransmitter) { h.dropFlow = true }},
		{name: "control rejection", want: "0x0314", configure: func(h *fakeTransmitter) { h.flowStatus = 0x0314 }},
		{name: "short flow handle", want: "need 6", configure: func(h *fakeTransmitter) { h.flowBody = []byte{1, 2} }},
		{name: "channel ports", want: "different transmitter", configure: func(h *fakeTransmitter) { h.channels[1].port++ }},
		{name: "channel encoding", want: "inconsistent encoding", configure: func(h *fakeTransmitter) { h.channels[1].bits = 24 }},
		{name: "unsupported rate", want: "unsupported sample rate", configure: func(h *fakeTransmitter) { h.channels[0].rate = 44100; h.channels[1].rate = 44100 }},
		{name: "mono transmitter", want: "need 2", configure: func(h *fakeTransmitter) { h.channels[0].nchan = 1; h.channels[1].nchan = 1 }},
		{name: "unsupported bits", want: "unsupported bits", configure: func(h *fakeTransmitter) { h.channels[0].bits = 8; h.channels[1].bits = 8 }},
		{name: "FPP exceeds MTU", want: "exceeds the MTU budget", configure: func(h *fakeTransmitter) { h.channels[0].fppMax = 500; h.channels[0].fppMin = 400 }},
		{name: "FPP has no common bounds", want: "minimum of 64", configure: func(h *fakeTransmitter) {
			h.channels[0].fppMax = 128
			h.channels[0].fppMin = 64
			h.channels[1].fppMax = 32
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				tc.configure(h)
				h.start(t)
				input := tc.input
				if input == "" {
					input = testInput
				}
				r, err := Open(t.Context(), input)
				if r != nil {
					_ = r.Close()
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Open = %v, %v; want %q", r, err, tc.want)
				}
				if len(h.stops) != 0 {
					t.Fatal("failed setup tried to stop an unestablished flow")
				}
			})
		})
	}
}

func TestOpenChoosesCommonFPPWithinMTU(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		bits, leftMax, rightMax, leftMin, rightMin, want uint16
	}{
		{name: "16-bit MTU", bits: 16, leftMax: 600, rightMax: 500, leftMin: 64, rightMin: 128, want: 350},
		{name: "24-bit MTU", bits: 24, leftMax: 600, rightMax: 500, leftMin: 64, rightMin: 128, want: 233},
		{name: "32-bit MTU", bits: 32, leftMax: 600, rightMax: 500, leftMin: 64, rightMin: 128, want: 175},
		{name: "common channel bounds", bits: 16, leftMax: 64, rightMax: 48, leftMin: 16, rightMin: 32, want: 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newFakeTransmitter()
				defer h.close(t)
				h.channels[0].bits, h.channels[1].bits = tc.bits, tc.bits
				h.channels[0].fppMax, h.channels[1].fppMax = tc.leftMax, tc.rightMax
				h.channels[0].fppMin, h.channels[1].fppMin = tc.leftMin, tc.rightMin
				h.start(t)
				r, err := Open(t.Context(), testInput)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Close() }()
				flow := <-h.flows
				if fpp := binary.BigEndian.Uint16(flow.request[controlHeaderSize+26:]); fpp != tc.want {
					t.Fatalf("chosen FPP = %d, want %d within both channels' bounds and MTU", fpp, tc.want)
				}
			})
		})
	}
}
