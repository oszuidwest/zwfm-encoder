// Package audio provides audio processing utilities including level metering and silence detection.
package audio

import (
	"encoding/binary"
	"math"
)

const (
	// MinDB is the minimum dB level (silence).
	MinDB = -60.0
	// MaxSampleValue is the maximum absolute value for 16-bit signed audio.
	MaxSampleValue = 32768.0
	// ClipThreshold is slightly below max to catch near-clips.
	ClipThreshold int16 = 32760
)

// LevelData holds raw sample accumulator data for level calculation.
type LevelData struct {
	SumSquaresL float64
	SumSquaresR float64
	PeakL       float64
	PeakR       float64
	ClipCountL  int
	ClipCountR  int
	SampleCount int
}

// ProcessSamples processes S16LE stereo PCM data and accumulates level data.
func ProcessSamples(buf []byte, n int, data *LevelData) {
	for i := 0; i+3 < n; i += 4 {
		leftSample := int16(binary.LittleEndian.Uint16(buf[i:]))
		rightSample := int16(binary.LittleEndian.Uint16(buf[i+2:]))
		left := float64(leftSample)
		right := float64(rightSample)

		data.SumSquaresL += left * left
		data.SumSquaresR += right * right

		if absL := math.Abs(left); absL > data.PeakL {
			data.PeakL = absL
		}
		if absR := math.Abs(right); absR > data.PeakR {
			data.PeakR = absR
		}

		if leftSample >= ClipThreshold || leftSample <= -ClipThreshold {
			data.ClipCountL++
		}
		if rightSample >= ClipThreshold || rightSample <= -ClipThreshold {
			data.ClipCountR++
		}

		data.SampleCount++
	}
}

// Levels contains calculated audio levels in dB.
type Levels struct {
	RMSLeft   float64
	RMSRight  float64
	PeakLeft  float64
	PeakRight float64
	ClipLeft  int
	ClipRight int
}

// CalculateLevels computes RMS and peak levels from accumulated sample data.
func CalculateLevels(data *LevelData) Levels {
	if data.SampleCount == 0 {
		return Levels{
			RMSLeft: MinDB, RMSRight: MinDB,
			PeakLeft: MinDB, PeakRight: MinDB,
		}
	}

	rmsL := math.Sqrt(data.SumSquaresL / float64(data.SampleCount))
	rmsR := math.Sqrt(data.SumSquaresR / float64(data.SampleCount))

	// Convert to dB (reference: MaxSampleValue for 16-bit audio)
	dbL := 20 * math.Log10(rmsL/MaxSampleValue)
	dbR := 20 * math.Log10(rmsR/MaxSampleValue)
	peakDbL := 20 * math.Log10(data.PeakL/MaxSampleValue)
	peakDbR := 20 * math.Log10(data.PeakR/MaxSampleValue)

	return Levels{
		RMSLeft:   max(dbL, MinDB),
		RMSRight:  max(dbR, MinDB),
		PeakLeft:  max(peakDbL, MinDB),
		PeakRight: max(peakDbR, MinDB),
		ClipLeft:  data.ClipCountL,
		ClipRight: data.ClipCountR,
	}
}

// Reset resets accumulators for the next measurement period.
func (d *LevelData) Reset() {
	d.SampleCount = 0
	d.SumSquaresL = 0
	d.SumSquaresR = 0
	d.PeakL = 0
	d.PeakR = 0
	d.ClipCountL = 0
	d.ClipCountR = 0
}
