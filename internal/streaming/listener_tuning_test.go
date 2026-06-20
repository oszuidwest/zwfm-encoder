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
		// PCM s302m at 240000 B/s for 2s = 480000 B, ceil(/4096) = 118 chunks.
		{name: "pcm fills 2s", codec: types.CodecPCM, want: 118},
		// Opus default 128k = 16000 B/s for 2s = 32000 B, ceil(/4096) = 8 = the floor.
		{name: "opus default floored", codec: types.CodecOpus, want: minListenerQueueChunks},
		// Opus 256k = 32000 B/s for 2s = 64000 B, ceil(/4096) = 16 chunks.
		{name: "opus 256k", codec: types.CodecOpus, bitrate: 256, want: 16},
		// MP3 default 320k = 40000 B/s for 2s = 80000 B, ceil(/4096) = 20 chunks.
		{name: "mp3 default", codec: types.CodecMP3, want: 20},
		// MP3 64k = 8000 B/s for 2s = 16000 B, ceil(/4096) = 4, floored to the minimum.
		{name: "mp3 low floored", codec: types.CodecMP3, bitrate: 64, want: minListenerQueueChunks},
		// MP3 5000k (above the validated range) = 1250000 B for 2s, ceil(/4096) = 306,
		// capped at the maximum. Guards the ceiling against unvalidated input.
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
		// PCM uses the s302m wire rate (192000 * 20/16), not the raw capture rate.
		{name: "pcm s302m wire rate", codec: types.CodecPCM, want: 240000},
		// Compressed codecs derive from kbit/s: bitrate * 1000 / 8.
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
