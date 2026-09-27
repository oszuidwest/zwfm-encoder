package dante_test

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/dante"
)

// Temporary live smoke test against a real Dante transmitter.
func TestLiveSmoke(t *testing.T) {
	input := os.Getenv("DANTE_LIVE_INPUT")
	if input == "" {
		t.Skip("DANTE_LIVE_INPUT not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	receiver, err := dante.Open(ctx, input)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Logf("opened in %v", time.Since(started))

	pcm := make([]byte, 3*48000*4)
	readStart := time.Now()
	if _, err := io.ReadFull(receiver, pcm); err != nil {
		t.Fatalf("read: %v", err)
	}
	t.Logf("read 3 s of audio in %v", time.Since(readStart))

	for ch, name := range []string{"left", "right"} {
		var sumSquares float64
		var peak float64
		crossings := 0
		prev := 0.0
		frames := len(pcm) / 4
		for f := range frames {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[f*4+ch*2:]))) / 32768 //nolint:gosec // S16LE sample reinterpretation.
			sumSquares += v * v
			peak = math.Max(peak, math.Abs(v))
			if f > 0 && prev < 0 && v >= 0 {
				crossings++
			}
			prev = v
		}
		rms := math.Sqrt(sumSquares / float64(frames))
		t.Logf("%s: peak %.1f dBFS, rms %.1f dBFS, ~%.0f Hz", name,
			20*math.Log10(peak), 20*math.Log10(rms), float64(crossings)/3)
	}

	cancel()
	if err := receiver.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	t.Logf("closed cleanly")
}
