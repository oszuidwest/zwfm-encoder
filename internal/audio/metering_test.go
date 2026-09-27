package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

const levelToleranceDB = 0.001

func makeKnownStereoPCM(frames int, sampleAt func(int) (int16, int16)) []byte {
	buf := make([]byte, frames*bytesPerFrame)
	for frame := range frames {
		left, right := sampleAt(frame)
		//nolint:gosec // Intentional signed PCM bit-pattern conversion.
		binary.LittleEndian.PutUint16(buf[frame*bytesPerFrame:], uint16(left))
		//nolint:gosec // Intentional signed PCM bit-pattern conversion.
		binary.LittleEndian.PutUint16(buf[frame*bytesPerFrame+2:], uint16(right))
	}
	return buf
}

func checkLevelNear(t *testing.T, field string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > levelToleranceDB {
		t.Errorf("%s = %.6f dBFS, want %.6f dBFS (+/- %.3f dB)", field, got, want, levelToleranceDB)
	}
}

func makeStereoPCM(frames int) []byte {
	buf := make([]byte, frames*bytesPerFrame)
	var b byte = 1
	for i := range buf {
		buf[i] = b
		b += 7 // Wraps mod 256; period is 256 because gcd(7, 256) == 1.
	}
	return buf
}

func feedInChunks(data *LevelData, pcm []byte, sizes []int) {
	pos, si := 0, 0
	for pos < len(pcm) {
		end := pos + sizes[si%len(sizes)]
		si++
		if end > len(pcm) {
			end = len(pcm)
		}
		ProcessSamples(pcm[pos:end], data)
		pos = end
	}
}

func TestCalculateLevelsAbsoluteCalibrationAndOrientation(t *testing.T) {
	t.Parallel()

	const (
		amplitude     int16   = 16384
		peakDBFS              = -6.020599913279624 // 20*log10(16384/32768)
		sineRMSDBFS           = -9.030899869919436 // peak minus 3.01 dB for a sine
		sineFrequency float64 = 1000
	)

	sine := func(frame int) int16 {
		phase := 2 * math.Pi * sineFrequency * float64(frame) / float64(SampleRate)
		return int16(math.Round(float64(amplitude) * math.Sin(phase)))
	}
	zero := func(int) int16 { return 0 }

	tests := []struct {
		name      string
		left      func(int) int16
		right     func(int) int16
		wantRMSL  float64
		wantRMSR  float64
		wantPeakL float64
		wantPeakR float64
	}{
		{
			name:      "sine left only",
			left:      sine,
			right:     zero,
			wantRMSL:  sineRMSDBFS,
			wantRMSR:  MinDB,
			wantPeakL: peakDBFS,
			wantPeakR: MinDB,
		},
		{
			name:      "sine right only",
			left:      zero,
			right:     sine,
			wantRMSL:  MinDB,
			wantRMSR:  sineRMSDBFS,
			wantPeakL: MinDB,
			wantPeakR: peakDBFS,
		},
		{
			name:      "digital silence",
			left:      zero,
			right:     zero,
			wantRMSL:  MinDB,
			wantRMSR:  MinDB,
			wantPeakL: MinDB,
			wantPeakR: MinDB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pcm := makeKnownStereoPCM(SampleRate, func(frame int) (int16, int16) {
				return tt.left(frame), tt.right(frame)
			})
			var data LevelData
			ProcessSamples(pcm, &data)
			got := CalculateLevels(&data)

			checkLevelNear(t, "RMSLeft", got.RMSLeft, tt.wantRMSL)
			checkLevelNear(t, "RMSRight", got.RMSRight, tt.wantRMSR)
			checkLevelNear(t, "PeakLeft", got.PeakLeft, tt.wantPeakL)
			checkLevelNear(t, "PeakRight", got.PeakRight, tt.wantPeakR)
		})
	}
}

func TestProcessSamplesClipThresholdPerChannel(t *testing.T) {
	t.Parallel()

	justBelowPositive := ClipThreshold - 1
	justBelowNegative := -ClipThreshold + 1
	beyondPositive := ClipThreshold + 1
	beyondNegative := -ClipThreshold - 1
	tests := []struct {
		name      string
		frames    [][2]int16
		wantLeft  int
		wantRight int
	}{
		{
			name: "left channel",
			frames: [][2]int16{
				{ClipThreshold, 0},
				{-ClipThreshold, 0},
				{beyondPositive, 0},
				{beyondNegative, 0},
				{justBelowPositive, 0},
				{justBelowNegative, 0},
			},
			wantLeft: 4,
		},
		{
			name: "right channel",
			frames: [][2]int16{
				{0, ClipThreshold},
				{0, -ClipThreshold},
				{0, beyondPositive},
				{0, beyondNegative},
				{0, justBelowPositive},
				{0, justBelowNegative},
			},
			wantRight: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pcm := makeKnownStereoPCM(len(tt.frames), func(frame int) (int16, int16) {
				return tt.frames[frame][0], tt.frames[frame][1]
			})
			var data LevelData
			ProcessSamples(pcm, &data)
			got := CalculateLevels(&data)

			if got.ClipLeft != tt.wantLeft || got.ClipRight != tt.wantRight {
				t.Errorf(
					"clip counts = left %d, right %d; want left %d, right %d",
					got.ClipLeft,
					got.ClipRight,
					tt.wantLeft,
					tt.wantRight,
				)
			}
		})
	}
}

func TestProcessSamplesChunkingInvariance(t *testing.T) {
	const frames = 5000
	pcm := makeStereoPCM(frames)
	var ref LevelData
	ProcessSamples(pcm, &ref)
	refLevels := CalculateLevels(&ref)
	patterns := []struct {
		name  string
		sizes []int
	}{
		{"one byte at a time", []int{1}},
		{"three byte chunks", []int{3}},
		{"unaligned mixed", []int{1, 2, 3, 5, 7}},
		{"around frame size", []int{3, 4, 5}},
		{"large then unaligned", []int{19199, 1, 7}},
	}
	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			var got LevelData
			feedInChunks(&got, pcm, p.sizes)
			if got.remainderLen != 0 {
				t.Fatalf("leftover remainder after a frame-aligned total: got %d, want 0", got.remainderLen)
			}
			if got.SampleCount != ref.SampleCount {
				t.Fatalf("frame count drifted with chunking: got %d, want %d", got.SampleCount, ref.SampleCount)
			}
			if gotLevels := CalculateLevels(&got); gotLevels != refLevels {
				t.Errorf("levels depend on chunk boundaries:\n got  %+v\n want %+v", gotLevels, refLevels)
			}
		})
	}
}

func TestProcessSamplesSplitFrameClipCounting(t *testing.T) {
	cases := []struct {
		name      string
		frame     []byte
		wantClipL int
		wantClipR int
	}{
		{"left clips", []byte{0xFF, 0x7F, 0x64, 0x00}, 1, 0},
		{"right clips", []byte{0x64, 0x00, 0x00, 0x80}, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var aligned LevelData
			ProcessSamples(c.frame, &aligned)
			want := CalculateLevels(&aligned)
			if want.ClipLeft != c.wantClipL || want.ClipRight != c.wantClipR {
				t.Fatalf("setup: aligned clip counts L=%d R=%d, want L=%d R=%d",
					want.ClipLeft, want.ClipRight, c.wantClipL, c.wantClipR)
			}
			for split := 1; split <= 3; split++ {
				t.Run(fmt.Sprintf("split %d+%d", split, len(c.frame)-split), func(t *testing.T) {
					var d LevelData
					ProcessSamples(c.frame[:split], &d)
					ProcessSamples(c.frame[split:], &d)
					if d.remainderLen != 0 {
						t.Fatalf("frame not fully consumed: remainderLen=%d", d.remainderLen)
					}
					if got := CalculateLevels(&d); got != want {
						t.Errorf("split decode differs from aligned:\n got  %+v\n want %+v", got, want)
					}
				})
			}
		})
	}
}

func TestProcessSamplesCarriesTrailingPartialFrame(t *testing.T) {
	buf := makeStereoPCM(6)[:22] // Keeps 5 full frames and 2 trailing bytes.
	var d LevelData
	ProcessSamples(buf, &d)
	if d.SampleCount != 5 {
		t.Fatalf("expected 5 whole frames, got %d", d.SampleCount)
	}
	if d.remainderLen != 2 {
		t.Fatalf("expected 2 carried bytes, got %d", d.remainderLen)
	}
}

func TestResetPreservesRemainder(t *testing.T) {
	pcm := makeStereoPCM(3) // Holds 12 bytes, or 3 frames.
	var d LevelData
	ProcessSamples(pcm[:10], &d) // Leaves 2 full frames plus 2 carried bytes.
	if d.SampleCount != 2 {
		t.Fatalf("expected 2 accumulated frames, got %d", d.SampleCount)
	}
	if d.remainderLen != 2 {
		t.Fatalf("expected 2 carried bytes, got %d", d.remainderLen)
	}
	d.Reset()
	if d.SampleCount != 0 {
		t.Fatalf("Reset should clear the sample count, got %d", d.SampleCount)
	}
	if d.remainderLen != 2 {
		t.Fatalf("Reset must preserve the partial-frame remainder, got %d", d.remainderLen)
	}
	ProcessSamples(pcm[10:], &d) // Completes the third frame with the carried bytes.
	if d.SampleCount != 1 {
		t.Fatalf("expected the carried frame to complete, got %d", d.SampleCount)
	}
	if d.remainderLen != 0 {
		t.Fatalf("expected no leftover after completing the frame, got %d", d.remainderLen)
	}
	var ref LevelData
	ProcessSamples(pcm[8:12], &ref) // Decodes frame 2 in one aligned read.
	if got, want := CalculateLevels(&d), CalculateLevels(&ref); got != want {
		t.Errorf("carried frame decoded incorrectly:\n got  %+v\n want %+v", got, want)
	}
}

func TestProcessSamplesEmptyBufferKeepsRemainder(t *testing.T) {
	pcm := makeStereoPCM(1)
	var d LevelData
	ProcessSamples(pcm[:2], &d) // Carries 2 bytes without a complete frame.
	if d.SampleCount != 0 || d.remainderLen != 2 {
		t.Fatalf("setup failed: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}
	ProcessSamples(nil, &d)
	if d.SampleCount != 0 || d.remainderLen != 2 {
		t.Fatalf("empty read disturbed state: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}
	ProcessSamples(pcm[2:], &d) // Completes the frame.
	if d.SampleCount != 1 || d.remainderLen != 0 {
		t.Fatalf("frame did not complete: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}
}
