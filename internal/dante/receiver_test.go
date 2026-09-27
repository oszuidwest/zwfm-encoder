package dante

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestDecodeMediaPacket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bits    int
		payload []byte
		wantPCM []byte
	}{
		{
			name:    "16 bit",
			bits:    16,
			payload: []byte{0x12, 0x34, 0xab, 0xcd},
			wantPCM: []byte{0x34, 0x12, 0xcd, 0xab},
		},
		{
			name:    "24 bit",
			bits:    24,
			payload: []byte{0x12, 0x34, 0x56, 0xab, 0xcd, 0xef},
			wantPCM: []byte{0x34, 0x12, 0xcd, 0xab},
		},
		{
			name:    "32 bit",
			bits:    32,
			payload: []byte{0x12, 0x34, 0x56, 0x78, 0xab, 0xcd, 0xef, 0x01},
			wantPCM: []byte{0x34, 0x12, 0xcd, 0xab},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			packet := make([]byte, mediaHeaderLength+len(test.payload))
			binary.BigEndian.PutUint32(packet[1:5], 5)
			binary.BigEndian.PutUint32(packet[5:9], 123)
			copy(packet[mediaHeaderLength:], test.payload)

			got, err := decodeMediaPacket(packet, test.bits)
			if err != nil {
				t.Fatalf("decodeMediaPacket() error = %v", err)
			}
			if got.timestamp != 5*sampleRate+123 {
				t.Fatalf("timestamp = %d, want %d", got.timestamp, 5*sampleRate+123)
			}
			if got.frames != 1 || !bytes.Equal(got.pcm, test.wantPCM) {
				t.Fatalf("decoded packet = frames:%d pcm:%x, want frames:1 pcm:%x", got.frames, got.pcm, test.wantPCM)
			}
		})
	}
}

func TestPacketReordererRestoresOrder(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	reorderer := packetReorderer{wait: time.Second, pending: make(map[uint64]mediaPacket)}
	now := time.Now()
	packets := []mediaPacket{
		{timestamp: 100, frames: 2, pcm: []byte{1}},
		{timestamp: 104, frames: 2, pcm: []byte{3}},
		{timestamp: 102, frames: 2, pcm: []byte{2}},
	}
	for _, packet := range packets {
		if err := reorderer.add(&output, packet, now); err != nil {
			t.Fatalf("add() error = %v", err)
		}
	}
	if got := output.Bytes(); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("output = %v, want [1 2 3]", got)
	}
}

func TestPacketReordererFillsGapWithSilence(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	now := time.Now()
	reorderer := packetReorderer{wait: time.Millisecond, pending: make(map[uint64]mediaPacket)}
	if err := reorderer.add(&output, mediaPacket{timestamp: 100, frames: 2, pcm: []byte{1}}, now); err != nil {
		t.Fatalf("add first packet: %v", err)
	}
	if err := reorderer.add(&output, mediaPacket{timestamp: 104, frames: 2, pcm: []byte{2}}, now); err != nil {
		t.Fatalf("add packet after gap: %v", err)
	}
	if err := reorderer.flushGap(&output, now.Add(2*time.Millisecond)); err != nil {
		t.Fatalf("flushGap() error = %v", err)
	}

	want := append([]byte{1}, make([]byte, 2*outputChannels*2)...)
	want = append(want, 2)
	if got := output.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("output = %v, want %v", got, want)
	}
}

func TestDecodeMediaPacketRejectsMalformedPayload(t *testing.T) {
	t.Parallel()

	if _, err := decodeMediaPacket(make([]byte, mediaHeaderLength+5), 24); err == nil {
		t.Fatal("decodeMediaPacket() error = nil")
	}
}

func TestPCMBatchWriter(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer := &pcmBatchWriter{
		destination: &output,
		buffer:      make([]byte, 0, 4),
		targetSize:  4,
	}
	if _, err := writer.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output length before complete batch = %d, want 0", output.Len())
	}
	if _, err := writer.Write([]byte{4, 5, 6, 7, 8, 9}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := output.Bytes(); !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("batched output = %v", got)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := output.Bytes(); !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("flushed output = %v", got)
	}
}
