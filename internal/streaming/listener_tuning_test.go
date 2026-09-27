package streaming

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestListenerQueueBuffersTargetDurationWithinBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		codec   types.Codec
		bitrate int
	}{
		{name: "pcm", codec: types.CodecPCM},
		{name: "opus default", codec: types.CodecOpus},
		{name: "opus maximum", codec: types.CodecOpus, bitrate: 256},
		{name: "mp3 default", codec: types.CodecMP3},
		{name: "mp3 minimum", codec: types.CodecMP3, bitrate: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := &types.Stream{Codec: tt.codec, Bitrate: tt.bitrate}
			chunks := listenerQueueChunks(stream)
			if chunks < minListenerQueueChunks || chunks > maxListenerQueueChunks {
				t.Fatalf("listenerQueueChunks() = %d, want within [%d, %d]",
					chunks, minListenerQueueChunks, maxListenerQueueChunks)
			}

			bufferedBytes := chunks * listenerStdoutBufferSize
			targetBytes := listenerBytesPerSecond(stream) * int(listenerBufferDuration.Milliseconds()) / 1000
			if bufferedBytes < targetBytes {
				t.Fatalf("queue buffers %d bytes, want at least target %d bytes", bufferedBytes, targetBytes)
			}
		})
	}
}
