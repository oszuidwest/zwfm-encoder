package streaming

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// listenerBufferDuration is the audio each fan-out subscriber buffers before
	// dropping. It absorbs encoder flush bursts and scheduler jitter so high-bitrate
	// PCM keeps the same headroom that low-bitrate codecs already get from the same
	// chunk count.
	listenerBufferDuration = 2 * time.Second
	// minListenerQueueChunks floors the per-subscriber queue so low-bitrate codecs
	// still absorb a flush burst.
	minListenerQueueChunks = 8
	// maxListenerQueueChunks caps per-subscriber memory at
	// maxListenerQueueChunks * listenerStdoutBufferSize.
	maxListenerQueueChunks = 256
	// listenerLatencyPCM raises the SRT latency for PCM so retransmission and jitter
	// tolerance match its larger queue. Other codecs use srtfanout.DefaultLatency.
	listenerLatencyPCM = time.Second
	// pcmListenerBytesPerSecond is the encoded wire rate of the PCM listener path,
	// which the fan-out actually buffers. SMPTE 302M (s302m) stores each 16-bit
	// sample in 20 bits (4 AES3 framing bits), so the elementary rate is
	// audio.BytesPerSecond * 20/16 = 240 kB/s (1.92 Mbit/s, per README), ~1.25x the
	// raw capture rate. Sizing on the raw rate under-buffers PCM by ~25%.
	pcmListenerBytesPerSecond = audio.BytesPerSecond * 20 / 16
)

// listenerFanoutConfig builds the fan-out configuration for a listener stream,
// sizing the per-subscriber queue and SRT latency from the codec byte rate.
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

// listenerQueueChunks sizes the per-subscriber queue to hold at least
// listenerBufferDuration of audio at the codec byte rate, using the stdout read
// size as the chunk granularity, clamped to the min/max bounds.
func listenerQueueChunks(stream *types.Stream) int {
	bytesPerSec := listenerBytesPerSecond(stream)
	targetBytes := bytesPerSec * int(listenerBufferDuration.Milliseconds()) / 1000
	chunks := (targetBytes + listenerStdoutBufferSize - 1) / listenerStdoutBufferSize
	return min(max(chunks, minListenerQueueChunks), maxListenerQueueChunks)
}

// listenerLatency returns the SRT latency for a listener stream. PCM uses a longer
// latency to match its larger queue; other codecs fall back to the fan-out default.
func listenerLatency(stream *types.Stream) time.Duration {
	if stream.Codec == types.CodecPCM {
		return listenerLatencyPCM
	}
	return 0
}

// listenerBytesPerSecond estimates the encoded byte rate of a listener stream,
// i.e. the bytes the fan-out buffers. PCM uses the s302m-in-MPEG-TS rate (not the
// raw capture rate); compressed codecs use their configured or default bitrate.
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
