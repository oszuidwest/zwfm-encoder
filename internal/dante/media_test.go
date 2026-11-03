package dante

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

func TestMediaDecoding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		bits    uint16
		payload []byte
		want    []byte
	}{
		{
			name:    "16 bit",
			bits:    16,
			payload: []byte{0x12, 0x34, 0xab, 0xcd},
			want:    []byte{0x34, 0x12, 0xcd, 0xab},
		},
		{
			name:    "24 bit",
			bits:    24,
			payload: []byte{0x12, 0x34, 0x56, 0xab, 0xcd, 0xef},
			want:    []byte{0x34, 0x12, 0xcd, 0xab},
		},
		{
			name:    "32 bit",
			bits:    32,
			payload: []byte{0x12, 0x34, 0x56, 0x78, 0xab, 0xcd, 0xef, 0x01},
			want:    []byte{0x34, 0x12, 0xcd, 0xab},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			processor := newMediaProcessor(test.bits, &output)
			valid, err := processor.receive(mediaDatagram(0, test.payload), time.Now())
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if !valid {
				t.Fatal("well-formed datagram was rejected")
			}
			if !bytes.Equal(output.Bytes(), test.want) {
				t.Fatalf("output = %x, want %x", output.Bytes(), test.want)
			}
		})
	}
}

func TestMalformedMediaIsDropped(t *testing.T) {
	t.Parallel()
	invalidOffset := mediaDatagram(0, []byte{1, 2, 3, 4})
	binary.BigEndian.PutUint32(invalidOffset[5:9], sampleRate)
	tests := []struct {
		name     string
		bits     uint16
		datagram []byte
	}{
		{name: "short header", bits: 16, datagram: make([]byte, 8)},
		{name: "header only", bits: 16, datagram: make([]byte, 9)},
		{name: "partial frame", bits: 16, datagram: append(make([]byte, 9), 1, 2, 3)},
		{name: "partial 24-bit frame", bits: 24, datagram: append(make([]byte, 9), 1, 2, 3, 4)},
		{name: "sample offset outside second", bits: 16, datagram: invalidOffset},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			processor := newMediaProcessor(test.bits, &output)
			valid, err := processor.receive(test.datagram, time.Now())
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if valid {
				t.Fatal("malformed datagram was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("malformed datagram produced %x", output.Bytes())
			}
		})
	}
}

func TestMediaReordering(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var output bytes.Buffer
	processor := newMediaProcessor(16, &output)
	packets := []struct {
		position uint32
		value    byte
	}{
		{position: 0, value: 1},
		{position: 2, value: 3},
		{position: 1, value: 2},
	}
	for _, packet := range packets {
		if _, err := processor.receive(samplePacket(packet.position, packet.value), now); err != nil {
			t.Fatal(err)
		}
	}
	want := append(sampleOutput(1), sampleOutput(2)...)
	want = append(want, sampleOutput(3)...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("output = %x, want %x", output.Bytes(), want)
	}
}

func TestMediaGapFilling(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var output bytes.Buffer
	processor := newMediaProcessor(16, &output)
	if _, err := processor.receive(samplePacket(0, 1), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(2, 3), now); err != nil {
		t.Fatal(err)
	}
	if err := processor.advance(now.Add(gapWait)); err != nil {
		t.Fatal(err)
	}
	want := append(sampleOutput(1), []byte{0, 0, 0, 0}...)
	want = append(want, sampleOutput(3)...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("output = %x, want %x", output.Bytes(), want)
	}
}

func TestMediaGapTimerSurvivesForwardProgress(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var output bytes.Buffer
	processor := newMediaProcessor(16, &output)
	if _, err := processor.receive(samplePacket(0, 1), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(3, 4), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(1, 2), now.Add(gapWait)); err != nil {
		t.Fatal(err)
	}
	want := append(sampleOutput(1), sampleOutput(2)...)
	want = append(want, []byte{0, 0, 0, 0}...)
	want = append(want, sampleOutput(4)...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("output = %x, want %x", output.Bytes(), want)
	}
}

func TestMediaGapTimerFollowsLongestWaitingPacket(t *testing.T) {
	t.Parallel()
	const packetFrames = 32
	packet := func(index uint32, value byte) []byte {
		return mediaDatagram(index*packetFrames, bytes.Repeat([]byte{value, 0, value, 0}, packetFrames))
	}
	type step struct {
		at       time.Duration
		datagram []byte // nil only advances the clock
	}
	var everySecondLost []step
	for index := uint32(0); index < sampleRate/packetFrames; index += 2 {
		at := time.Duration(index) * packetFrames * time.Second / sampleRate
		everySecondLost = append(everySecondLost, step{at, packet(index, 1)})
	}
	var burstOutput []byte
	for value := byte(1); value <= 5; value++ {
		burstOutput = append(burstOutput, bytes.Repeat(sampleOutput(value), packetFrames)...)
	}
	silence := make([]byte, packetFrames*outputFrameSize)
	oldestArrivalOutput := slices.Concat(
		bytes.Repeat(sampleOutput(1), packetFrames), silence,
		bytes.Repeat(sampleOutput(2), packetFrames), silence,
		bytes.Repeat(sampleOutput(3), packetFrames),
	)
	tests := []struct {
		name  string
		steps []step
		want  []byte // nil when only the bounds matter
	}{
		{
			name: "burst keeps the reordered packet",
			steps: []step{
				{0, packet(0, 1)},
				{100 * time.Microsecond, packet(2, 3)},
				{3 * time.Millisecond, packet(4, 5)},
				{3500 * time.Microsecond, packet(1, 2)},
				{4200 * time.Microsecond, nil},
				{4500 * time.Microsecond, packet(3, 4)},
			},
			want: burstOutput,
		},
		{
			name: "oldest arrival decides, not the lowest position",
			steps: []step{
				{0, packet(0, 1)},
				{time.Millisecond, packet(4, 3)},
				{3 * time.Millisecond, packet(2, 2)},
				{5 * time.Millisecond, nil},
			},
			want: oldestArrivalOutput,
		},
		{name: "sustained loss stays bounded", steps: everySecondLost},
	}
	const gapFrames = int(gapWait * sampleRate / time.Second)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			var output bytes.Buffer
			processor := newMediaProcessor(16, &output)
			for _, step := range test.steps {
				now := start.Add(step.at)
				if step.datagram == nil {
					if err := processor.advance(now); err != nil {
						t.Fatal(err)
					}
				} else if valid, err := processor.receive(step.datagram, now); err != nil || !valid {
					t.Fatalf("receive at %v = (%t, %v), want (true, nil)", step.at, valid, err)
				}
				lag := int(step.at*sampleRate/time.Second) - output.Len()/outputFrameSize
				if lag > gapFrames+packetFrames {
					t.Fatalf("at %v the output lags %d frames", step.at, lag)
				}
			}
			if test.want != nil && !bytes.Equal(output.Bytes(), test.want) {
				t.Fatalf("output = %x, want %x", output.Bytes(), test.want)
			}
		})
	}
}

func TestMediaRejectsGapLargerThanOneSecond(t *testing.T) {
	t.Parallel()
	now := time.Now()
	processor := newMediaProcessor(16, &bytes.Buffer{})
	if _, err := processor.receive(samplePacket(0, 1), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(maximumGapInFrames+2, 2), now); err != nil {
		t.Fatal(err)
	}
	if err := processor.advance(now.Add(gapWait)); err == nil {
		t.Fatal("gap larger than one second did not fail")
	}
}

func TestMediaOverlapDiscarding(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var output bytes.Buffer
	processor := newMediaProcessor(16, &output)
	if _, err := processor.receive(twoFramePacket(0, 1, 2), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(1, 9), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(samplePacket(3, 9), now); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.receive(twoFramePacket(2, 3, 4), now); err != nil {
		t.Fatal(err)
	}
	want := append(sampleOutput(1), sampleOutput(2)...)
	want = append(want, sampleOutput(3)...)
	want = append(want, sampleOutput(4)...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("output = %x, want %x", output.Bytes(), want)
	}
}

func TestMediaRejectsStalePackets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	processor := newMediaProcessor(16, &bytes.Buffer{})
	if _, err := processor.receive(samplePacket(1, 1), now); err != nil {
		t.Fatal(err)
	}
	valid, err := processor.receive(samplePacket(0, 9), now)
	if err != nil || valid {
		t.Fatalf("late packet = (%t, %v), want (false, nil)", valid, err)
	}
	if _, err := processor.receive(samplePacket(3, 3), now); err != nil {
		t.Fatal(err)
	}
	valid, err = processor.receive(samplePacket(3, 9), now)
	if err != nil || valid {
		t.Fatalf("duplicate packet = (%t, %v), want (false, nil)", valid, err)
	}
}

func TestReceiveMediaDropsOtherSources(t *testing.T) {
	t.Parallel()
	mediaConn := listenLoopbackUDP(t)
	defer func() { _ = mediaConn.Close() }()
	trusted := listenLoopbackUDP(t)
	defer func() { _ = trusted.Close() }()
	rogue, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Skipf("open second loopback address: %v", err)
	}
	defer func() { _ = rogue.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stopInterrupt := context.AfterFunc(ctx, func() { _ = mediaConn.Close() })
	defer stopInterrupt()
	outputReader, outputWriter := io.Pipe()
	result := make(chan error, 1)
	go func() {
		receiveErr := receiveMedia(
			ctx,
			mediaConn,
			trusted.LocalAddr().(*net.UDPAddr).IP,
			16,
			outputWriter,
		)
		_ = outputWriter.CloseWithError(receiveErr)
		result <- receiveErr
	}()

	target := mediaConn.LocalAddr().(*net.UDPAddr)
	if _, err := rogue.WriteToUDP(samplePacket(0, 9), target); err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.WriteToUDP(samplePacket(0, 1), target); err != nil {
		t.Fatal(err)
	}
	want := sampleOutput(1)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(outputReader, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output = %x, want trusted source %x", got, want)
	}
	cancel()
	_ = mediaConn.Close()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("receiveMedia returned %v, want context cancellation", err)
	}
}

func mediaDatagram(position uint32, payload []byte) []byte {
	datagram := make([]byte, mediaHeaderSize+len(payload))
	binary.BigEndian.PutUint32(datagram[1:5], position/sampleRate)
	binary.BigEndian.PutUint32(datagram[5:9], position%sampleRate)
	copy(datagram[mediaHeaderSize:], payload)
	return datagram
}

func samplePacket(position uint32, value byte) []byte {
	return mediaDatagram(position, []byte{value, 0, value, 0})
}

func twoFramePacket(position uint32, first, second byte) []byte {
	payload := append([]byte{}, first, 0, first, 0)
	payload = append(payload, second, 0, second, 0)
	return mediaDatagram(position, payload)
}

func sampleOutput(value byte) []byte {
	return []byte{0, value, 0, value}
}
