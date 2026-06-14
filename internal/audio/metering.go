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
	// ClipThreshold is the sample value at or above which audio is considered clipping.
	ClipThreshold int16 = 32760
	// bytesPerFrame is the size of one S16LE stereo frame: 2 channels x 2 bytes.
	bytesPerFrame = 4
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

	// remainder holds the trailing bytes (0-3) of the previous buffer that did
	// not complete a stereo frame. Pipe reads are not guaranteed to align to
	// frame boundaries, so these bytes are carried into the next ProcessSamples
	// call to keep frame decoding aligned with the PCM stream instead of
	// dropping them and drifting on every unaligned read.
	remainder    [bytesPerFrame - 1]byte
	remainderLen int
}

// ProcessSamples accumulates level data from a buffer of S16LE stereo PCM.
//
// It decodes complete 4-byte stereo frames and carries any trailing bytes that
// do not complete a frame into data, prepending them on the next call. Pipe
// reads are not guaranteed to land on frame boundaries (a POSIX pipe write
// larger than PIPE_BUF may be split), so carrying the remainder keeps the
// accumulation aligned with the PCM stream instead of dropping bytes and
// drifting on every unaligned read. The carried remainder survives Reset and is
// cleared only by allocating a fresh LevelData for a new capture session.
func ProcessSamples(buf []byte, n int, data *LevelData) {
	i := 0

	// Complete a frame whose bytes are split across the previous and current
	// buffer before processing the rest of this buffer on frame boundaries.
	if data.remainderLen > 0 {
		need := bytesPerFrame - data.remainderLen
		if n < need {
			data.remainderLen += copy(data.remainder[data.remainderLen:], buf[:n])
			return
		}
		var frame [bytesPerFrame]byte
		copy(frame[:], data.remainder[:data.remainderLen])
		copy(frame[data.remainderLen:], buf[:need])
		accumulateFrame(frame[:], data)
		i = need
		data.remainderLen = 0
	}

	// Accumulate every whole frame that fits in the rest of the buffer.
	for ; i+bytesPerFrame <= n; i += bytesPerFrame {
		accumulateFrame(buf[i:], data)
	}

	// Carry the trailing partial frame (0-3 bytes) into the next call.
	if i < n {
		data.remainderLen = copy(data.remainder[:], buf[i:n])
	}
}

// accumulateFrame decodes one S16LE stereo frame from the first 4 bytes of frame
// and folds it into the running level data.
func accumulateFrame(frame []byte, data *LevelData) {
	//nolint:gosec // Intentional reinterpretation of unsigned PCM to signed
	leftSample := int16(binary.LittleEndian.Uint16(frame[0:]))
	//nolint:gosec // Intentional reinterpretation of unsigned PCM to signed
	rightSample := int16(binary.LittleEndian.Uint16(frame[2:]))
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

// Levels contains calculated audio levels in dB.
type Levels struct {
	RMSLeft   float64
	RMSRight  float64
	PeakLeft  float64
	PeakRight float64
	ClipLeft  int
	ClipRight int
}

// CalculateLevels computes RMS and peak levels in dB from the given level data.
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

// Reset clears the accumulated level data so a new metering window can begin. It
// deliberately preserves the partial-frame remainder: that carry tracks frame
// alignment in the continuous PCM stream and is independent of the level-update
// window. The remainder is discarded only when a fresh LevelData is allocated for
// a new capture session.
func (d *LevelData) Reset() {
	d.SampleCount = 0
	d.SumSquaresL = 0
	d.SumSquaresR = 0
	d.PeakL = 0
	d.PeakR = 0
	d.ClipCountL = 0
	d.ClipCountR = 0
}
