package streaming

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// listenerBufferDuration is the target buffered audio per subscriber.
	// Queue depth is derived from codec byte rate so PCM absorbs encoder bursts.
	listenerBufferDuration = 2 * time.Second
	// minListenerQueueChunks keeps low-bitrate queues deep enough for bursts.
	minListenerQueueChunks = 8
	// maxListenerQueueChunks bounds memory per subscriber.
	maxListenerQueueChunks = 256
	// listenerLatencyPCM gives PCM retransmission time for its larger queue.
	listenerLatencyPCM = time.Second
	// pcmListenerBytesPerSecond estimates PCM after s302m framing.
	// SMPTE 302M carries each 16-bit sample in roughly 20 bits before MPEG-TS
	// overhead, so sizing on raw capture bytes would under-buffer PCM.
	pcmListenerBytesPerSecond = audio.BytesPerSecond * 20 / 16
)

// listenerFanoutConfig returns an SRT fan-out config tuned for a listener stream.
func listenerFanoutConfig(stream *types.Stream) srtfanout.Config {
	return srtfanout.Config{
		StreamID:    stream.ID,
		BindHost:    stream.ListenerBindHost(),
		Port:        stream.Port,
		Password:    stream.Password,
		QueueChunks: listenerQueueChunks(stream),
		Latency:     listenerLatency(stream),
	}
}

// listenerQueueChunks returns the queue depth for listenerBufferDuration at the
// stream's encoded byte rate, clamped to min/max bounds.
func listenerQueueChunks(stream *types.Stream) int {
	bytesPerSec := listenerBytesPerSecond(stream)
	targetBytes := bytesPerSec * int(listenerBufferDuration.Milliseconds()) / 1000
	chunks := (targetBytes + listenerStdoutBufferSize - 1) / listenerStdoutBufferSize
	return min(max(chunks, minListenerQueueChunks), maxListenerQueueChunks)
}

// listenerLatency returns PCM-specific SRT latency or the fan-out default.
func listenerLatency(stream *types.Stream) time.Duration {
	if stream.Codec == types.CodecPCM {
		return listenerLatencyPCM
	}
	return srtfanout.DefaultLatency
}

// listenerBytesPerSecond returns the byte rate used to size listener queues.
// PCM uses s302m framing; compressed codecs use configured or default bitrate.
func listenerBytesPerSecond(stream *types.Stream) int {
	if stream.Codec == types.CodecPCM {
		return pcmListenerBytesPerSecond
	}
	kbit := stream.Bitrate
	if kbit <= 0 {
		kbit = stream.Codec.DefaultBitrate()
	}
	return kbit * 1000 / 8
}
