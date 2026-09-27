package dante

import (
	"bytes"
	"encoding/binary"
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
	tests := []struct {
		name     string
		bits     uint16
		datagram []byte
	}{
		{name: "short header", bits: 16, datagram: make([]byte, 8)},
		{name: "header only", bits: 16, datagram: make([]byte, 9)},
		{name: "partial frame", bits: 16, datagram: append(make([]byte, 9), 1, 2, 3)},
		{name: "partial 24-bit frame", bits: 24, datagram: append(make([]byte, 9), 1, 2, 3, 4)},
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

func mediaDatagram(position uint32, payload []byte) []byte {
	datagram := make([]byte, mediaHeaderSize+len(payload))
	binary.BigEndian.PutUint32(datagram[5:9], position)
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
