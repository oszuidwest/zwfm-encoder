package streaming

import (
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// listenerBufferDuration is the audio each fan-out subscriber buffers before
	// dropping. The queue is sized to hold this much audio at the codec byte rate
	// (clamped by the min/max bounds), absorbing encoder flush bursts and scheduler
	// jitter. It replaces the old fixed 2-chunk queue, which held only ~34 ms of
	// high-bitrate PCM.
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
	// pcmListenerBytesPerSecond approximates the s302m wire rate the fan-out buffers
	// for PCM. SMPTE 302M frames each 16-bit sample in ~20 bits (AES3 framing), so
	// the elementary rate is roughly audio.BytesPerSecond * 20/16 = 240 kB/s
	// (~1.92 Mbit/s, matching the README), about 1.25x the raw capture rate. This is
	// a sizing approximation, not an exact wire measurement (MPEG-TS adds further
	// overhead); sizing on the raw rate would under-buffer PCM by ~25%.
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
// latency to match its larger queue; other codecs use the fan-out default.
func listenerLatency(stream *types.Stream) time.Duration {
	if stream.Codec == types.CodecPCM {
		return listenerLatencyPCM
	}
	return srtfanout.DefaultLatency
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
