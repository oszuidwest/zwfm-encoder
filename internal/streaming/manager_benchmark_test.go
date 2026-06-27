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

// BenchmarkDistributorStreamFanOutCallerStreams measures shared-copy fan-out
// for caller-mode streams. The path allocates one PCM copy per chunk regardless
// of stream count.
func BenchmarkDistributorStreamFanOutCallerStreams(b *testing.B) {
	pcm := benchmarkPCMChunk()
	for _, count := range []int{1, 4, 16} {
		m := NewManager("ffmpeg")
		chans := make([]chan []byte, count)
		for i := range count {
			id := fmt.Sprintf("stream-%02d", i)
			ch := make(chan []byte, audioBufferSize)
			m.streams[id] = &Stream{
				state:   types.ProcessRunning,
				mode:    types.StreamModeCaller,
				audioCh: ch,
			}
			chans[i] = ch
		}

		b.Run(fmt.Sprintf("streams=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m.WriteAudioFanOut(pcm)
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

		b.ReportAllocs()
		for b.Loop() {
			m.WriteAudioFanOut(pcm)
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
			m.WriteAudioFanOut(pcm)
			<-run.audioCh
		}
	})
}
