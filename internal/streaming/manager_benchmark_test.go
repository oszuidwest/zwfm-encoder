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

// BenchmarkDistributorStreamFanOutCallerStreams compares the per-stream clone
// path (WriteAudio called once per stream) against the shared-copy fan-out
// (WriteAudioFanOut) for caller-mode streams. The shared-copy path allocates
// one PCM copy per chunk regardless of stream count.
func BenchmarkDistributorStreamFanOutCallerStreams(b *testing.B) {
	pcm := benchmarkPCMChunk()
	for _, count := range []int{1, 4, 16} {
		m := NewManager("ffmpeg")
		streams := make([]types.Stream, count)
		chans := make([]chan []byte, count)
		for i := range count {
			id := fmt.Sprintf("stream-%02d", i)
			ch := make(chan []byte, audioBufferSize)
			m.streams[id] = &Stream{
				state:   types.ProcessRunning,
				mode:    types.StreamModeCaller,
				audioCh: ch,
			}
			streams[i] = types.Stream{ID: id}
			chans[i] = ch
		}

		b.Run(fmt.Sprintf("per-stream-clone/streams=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for i := range streams {
					_ = m.WriteAudio(streams[i].ID, pcm)
				}
				for _, ch := range chans {
					<-ch
				}
			}
		})

		b.Run(fmt.Sprintf("shared-copy/streams=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m.WriteAudioFanOut(streams, pcm)
				for _, ch := range chans {
					<-ch
				}
			}
		})
	}
}

// BenchmarkDistributorStreamFanOutListenerStreams covers the listener-mode
// fan-out path, including the no-encoder-run case that must not allocate.
func BenchmarkDistributorStreamFanOutListenerStreams(b *testing.B) {
	pcm := benchmarkPCMChunk()

	b.Run("encoder=nil", func(b *testing.B) {
		m := NewManager("ffmpeg")
		m.streams["listener-1"] = &Stream{
			state: types.ProcessRunning,
			mode:  types.StreamModeListener,
		}
		streams := []types.Stream{{ID: "listener-1"}}

		b.ReportAllocs()
		for b.Loop() {
			m.WriteAudioFanOut(streams, pcm)
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
		streams := []types.Stream{{ID: "listener-1"}}

		b.ReportAllocs()
		for b.Loop() {
			m.WriteAudioFanOut(streams, pcm)
			<-run.audioCh
		}
	})
}
