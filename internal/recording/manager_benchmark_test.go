package recording

import (
	"fmt"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const benchmarkPCMChunkSize = 20 * 1024

func benchmarkPCMChunk() []byte {
	return make([]byte, benchmarkPCMChunkSize)
}

func BenchmarkDistributorRecorderFanOut(b *testing.B) {
	pcm := benchmarkPCMChunk()
	for _, count := range []int{0, 1, 4, 16} {
		b.Run(fmt.Sprintf("recorders=%d", count), func(b *testing.B) {
			m := &Manager{recorders: make(map[string]*GenericRecorder, count)}
			recorders := make([]*GenericRecorder, count)
			for i := range count {
				id := fmt.Sprintf("recorder-%02d", i)
				recorder := &GenericRecorder{
					id:      id,
					state:   types.ProcessRunning,
					audioCh: make(chan []byte, 1),
				}
				m.recorders[id] = recorder
				recorders[i] = recorder
			}

			b.ReportAllocs()
			for b.Loop() {
				m.WriteAudio(pcm)
				for _, recorder := range recorders {
					<-recorder.audioCh
				}
			}
		})
	}
}
