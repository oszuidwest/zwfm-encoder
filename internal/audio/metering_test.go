package audio

import (
	"encoding/binary"
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
