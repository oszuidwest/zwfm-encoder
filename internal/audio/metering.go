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
// Format: [L_low, L_high, R_low, R_high, ...].
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
	RMSL  float64
	RMSR  float64
	PeakL float64
	PeakR float64
	ClipL int
	ClipR int
}

// CalculateLevels returns audio levels in dB from accumulated sample data.
func CalculateLevels(data *LevelData) Levels {
	if data.SampleCount == 0 {
		return Levels{
			RMSL: MinDB, RMSR: MinDB,
			PeakL: MinDB, PeakR: MinDB,
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
		RMSL:  max(dbL, MinDB),
		RMSR:  max(dbR, MinDB),
		PeakL: max(peakDbL, MinDB),
		PeakR: max(peakDbR, MinDB),
		ClipL: data.ClipCountL,
		ClipR: data.ClipCountR,
	}
}

// ResetLevelData resets all level data to zero.
func ResetLevelData(data *LevelData) {
	data.SampleCount = 0
	data.SumSquaresL = 0
	data.SumSquaresR = 0
	data.PeakL = 0
	data.PeakR = 0
	data.ClipCountL = 0
	data.ClipCountR = 0
}
