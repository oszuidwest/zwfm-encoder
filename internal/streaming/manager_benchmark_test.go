package streaming

import (
	"fmt"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const benchmarkPCMChunkSize = 20 * 1024

func benchmarkPCMChunk() []byte {
	return make([]byte, benchmarkPCMChunkSize)
}

func BenchmarkDistributorStreamFanOutCallerStreams(b *testing.B) {
	pcm := benchmarkPCMChunk()
	for _, count := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("streams=%d", count), func(b *testing.B) {
			m := NewManager("ffmpeg")
			ids := make([]string, count)
			streams := make([]*Stream, count)
			for i := range count {
				id := fmt.Sprintf("stream-%02d", i)
				stream := &Stream{
					state:   types.ProcessRunning,
					mode:    types.StreamModeCaller,
					audioCh: make(chan []byte, audioBufferSize),
				}
				ids[i] = id
				streams[i] = stream
				m.streams[id] = stream
			}

			b.ReportAllocs()
			for b.Loop() {
				for _, id := range ids {
					_ = m.WriteAudio(id, pcm)
				}
				for _, stream := range streams {
					<-stream.audioCh
				}
			}
		})
	}
}

func BenchmarkDistributorStreamFanOutListenerStreams(b *testing.B) {
	pcm := benchmarkPCMChunk()

	b.Run("encoder=nil", func(b *testing.B) {
		m := NewManager("ffmpeg")
		m.streams["listener-1"] = &Stream{
			state: types.ProcessRunning,
			mode:  types.StreamModeListener,
		}

		b.ReportAllocs()
		for b.Loop() {
			_ = m.WriteAudio("listener-1", pcm)
		}
	})

	b.Run("encoder=active", func(b *testing.B) {
		m := NewManager("ffmpeg")
		run := &encoderRun{audioCh: make(chan []byte, audioBufferSize)}
		stream := &Stream{
			state: types.ProcessRunning,
			mode:  types.StreamModeListener,
		}
		stream.encoder = run
		m.streams["listener-1"] = stream

		b.ReportAllocs()
		for b.Loop() {
			_ = m.WriteAudio("listener-1", pcm)
			<-run.audioCh
		}
	})
}
