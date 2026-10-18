//go:build integration

package dante_test

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/dante"
)

// TestInfernoInteroperability requires an independent Inferno sender playing
// 48kHz stereo tones: left 440Hz, right 880Hz, both with amplitude 0.25.
func TestInfernoInteroperability(t *testing.T) {
	input := os.Getenv("DANTE_INFERNO_INPUT")
	if input == "" {
		t.Skip("DANTE_INFERNO_INPUT not set")
	}
	for _, tc := range []struct {
		name, input string
		tones       [2]float64
	}{
		{name: "stereo", input: input, tones: [2]float64{440, 880}},
		{name: "reopen", input: input, tones: [2]float64{440, 880}},
		{name: "swapped", input: strings.Replace(input, "/TX%201/TX%202", "/TX%202/TX%201", 1), tones: [2]float64{880, 440}},
		{name: "automatic interface", input: strings.Split(input, "?")[0], tones: [2]float64{440, 880}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			r, err := dante.Open(ctx, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if _, err := io.CopyN(io.Discard, r, 48000*4); err != nil {
				t.Fatalf("warmup: %v", err)
			}
			const seconds = 5
			pcm := make([]byte, seconds*48000*4)
			started := time.Now()
			if _, err := io.ReadFull(r, pcm); err != nil {
				t.Fatalf("media: %v", err)
			}
			if elapsed := time.Since(started); elapsed < 4*time.Second || elapsed > 7*time.Second {
				t.Errorf("5 seconds of PCM took %s", elapsed)
			}
			for channel, expected := range tc.tones {
				var squares float64
				var previous int16
				crossings := 0
				for frame := range len(pcm) / 4 {
					sample := int16(binary.LittleEndian.Uint16(pcm[frame*4+channel*2:])) //nolint:gosec // Interpret signed S16LE PCM.
					squares += math.Pow(float64(sample)/32768, 2)
					if previous < 0 && sample >= 0 {
						crossings++
					}
					previous = sample
				}
				frequency := float64(crossings) / seconds
				rms := 20 * math.Log10(math.Sqrt(squares/(seconds*48000)))
				t.Logf("channel %d: %.2f Hz, %.2f dBFS RMS", channel+1, frequency, rms)
				if math.Abs(frequency-expected) > expected*0.02 || rms < -16 || rms > -14 {
					t.Errorf("channel %d: want %.0f Hz within 2%% and -15.05 dBFS within 1 dB", channel+1, expected)
				}
			}
			cancel()
			if err := r.Wait(); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		})
	}
}
