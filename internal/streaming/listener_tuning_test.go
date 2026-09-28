package streaming

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestListenerQueueBuffersTargetDurationWithinBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		codec   types.Codec
		bitrate int
		want    int
	}{
		// PCM uses the s302m rate, not raw capture bytes.
		// PCM: ceil(240000 B/s * 2s / 4096) = 118 chunks.
		{name: "pcm", codec: types.CodecPCM, want: 118},
		// Opus default lands exactly on the floor.
		{name: "opus default", codec: types.CodecOpus, want: 8},
		// Opus 256k: ceil(32000 B/s * 2s / 4096) = 16 chunks.
		{name: "opus maximum", codec: types.CodecOpus, bitrate: 256, want: 16},
		// MP3 default: ceil(40000 B/s * 2s / 4096) = 20 chunks.
		{name: "mp3 default", codec: types.CodecMP3, want: 20},
		// MP3 64k computes below the floor.
		{name: "mp3 minimum", codec: types.CodecMP3, bitrate: 64, want: 8},
		// A deliberately excessive bitrate is capped at the maximum.
		{name: "maximum clamp", codec: types.CodecMP3, bitrate: 10000, want: maxListenerQueueChunks},
	}
	// Compressed codecs use kbit/s * 1000 / 8.
	// Queue sizing remains clamped between the minimum and maximum chunk counts.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := &types.Stream{Codec: tt.codec, Bitrate: tt.bitrate}
			chunks := listenerQueueChunks(stream)
			if chunks != tt.want {
				t.Fatalf("listenerQueueChunks() = %d, want %d", chunks, tt.want)
			}

			bufferedBytes := chunks * listenerStdoutBufferSize
			targetBytes := listenerBytesPerSecond(stream) * int(listenerBufferDuration.Milliseconds()) / 1000
			if chunks < maxListenerQueueChunks && bufferedBytes < targetBytes {
				t.Fatalf("queue buffers %d bytes, want at least target %d bytes", bufferedBytes, targetBytes)
			}
		})
	}
}

func TestListenerLatencyRaisedForPCMOnly(t *testing.T) {
	t.Parallel()
	if got := listenerLatency(&types.Stream{Codec: types.CodecPCM}); got != listenerLatencyPCM {
		t.Fatalf("listenerLatency(pcm) = %s, want %s", got, listenerLatencyPCM)
	}
	if got := listenerLatency(&types.Stream{Codec: types.CodecMP3}); got != srtfanout.DefaultLatency {
		t.Fatalf("listenerLatency(mp3) = %s, want %s", got, srtfanout.DefaultLatency)
	}
}
