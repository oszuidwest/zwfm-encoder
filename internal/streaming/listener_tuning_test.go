package streaming

import (
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestListenerQueueChunks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		codec   types.Codec
		bitrate int
		want    int
	}{
		// PCM: ceil(240000 B/s * 2s / 4096) = 118 chunks.
		{name: "pcm fills 2s", codec: types.CodecPCM, want: 118},
		// Opus default lands exactly on the floor.
		{name: "opus default floored", codec: types.CodecOpus, want: minListenerQueueChunks},
		// Opus 256k: ceil(32000 B/s * 2s / 4096) = 16 chunks.
		{name: "opus 256k", codec: types.CodecOpus, bitrate: 256, want: 16},
		// MP3 default: ceil(40000 B/s * 2s / 4096) = 20 chunks.
		{name: "mp3 default", codec: types.CodecMP3, want: 20},
		// MP3 64k computes below the floor.
		{name: "mp3 low floored", codec: types.CodecMP3, bitrate: 64, want: minListenerQueueChunks},
		// Above-range MP3 guards the max clamp before validation.
		{name: "above max capped", codec: types.CodecMP3, bitrate: 5000, want: maxListenerQueueChunks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := &types.Stream{Codec: tt.codec, Bitrate: tt.bitrate}
			if got := listenerQueueChunks(stream); got != tt.want {
				t.Fatalf("listenerQueueChunks(%s, %d) = %d, want %d", tt.codec, tt.bitrate, got, tt.want)
			}
		})
	}
}

func TestListenerQueueChunksWithinBounds(t *testing.T) {
	t.Parallel()
	for _, codec := range []types.Codec{types.CodecPCM, types.CodecOpus, types.CodecMP3} {
		got := listenerQueueChunks(&types.Stream{Codec: codec})
		if got < minListenerQueueChunks || got > maxListenerQueueChunks {
			t.Fatalf("listenerQueueChunks(%s) = %d, want within [%d,%d]",
				codec, got, minListenerQueueChunks, maxListenerQueueChunks)
		}
	}
}

func TestListenerBytesPerSecond(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		codec   types.Codec
		bitrate int
		want    int
	}{
		// PCM uses the s302m rate, not raw capture bytes.
		{name: "pcm s302m wire rate", codec: types.CodecPCM, want: 240000},
		// Compressed codecs use kbit/s * 1000 / 8.
		{name: "opus default", codec: types.CodecOpus, want: 16000},
		{name: "opus 256k", codec: types.CodecOpus, bitrate: 256, want: 32000},
		{name: "mp3 default", codec: types.CodecMP3, want: 40000},
		{name: "mp3 64k", codec: types.CodecMP3, bitrate: 64, want: 8000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := &types.Stream{Codec: tt.codec, Bitrate: tt.bitrate}
			if got := listenerBytesPerSecond(stream); got != tt.want {
				t.Fatalf("listenerBytesPerSecond(%s, %d) = %d, want %d", tt.codec, tt.bitrate, got, tt.want)
			}
		})
	}
}

func TestListenerLatency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		codec types.Codec
		want  time.Duration
	}{
		{name: "pcm raised", codec: types.CodecPCM, want: listenerLatencyPCM},
		{name: "opus default", codec: types.CodecOpus, want: srtfanout.DefaultLatency},
		{name: "mp3 default", codec: types.CodecMP3, want: srtfanout.DefaultLatency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := listenerLatency(&types.Stream{Codec: tt.codec}); got != tt.want {
				t.Fatalf("listenerLatency(%s) = %s, want %s", tt.codec, got, tt.want)
			}
		})
	}
}

func TestListenerFanoutConfigCarriesStreamFields(t *testing.T) {
	t.Parallel()
	stream := &types.Stream{
		ID:       "stream-test",
		Mode:     types.StreamModeListener,
		Host:     "127.0.0.1",
		Port:     9100,
		Password: "secret",
		Codec:    types.CodecPCM,
	}
	cfg := listenerFanoutConfig(stream)
	want := srtfanout.Config{
		StreamID:    "stream-test",
		BindHost:    stream.ListenerBindHost(),
		Port:        9100,
		Password:    "secret",
		QueueChunks: listenerQueueChunks(stream),
		Latency:     listenerLatencyPCM,
	}
	if cfg != want {
		t.Fatalf("listenerFanoutConfig() = %+v, want %+v", cfg, want)
	}
}
