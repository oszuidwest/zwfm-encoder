package audio

import (
	"encoding/binary"
	"sync"
	"time"
)

// WaveformPoints is the number of 10 ms points the waveform keeps (10 s).
const WaveformPoints = 10 * 100

const (
	waveformBucketFrames = SampleRate / 100
	waveformBucket       = time.Second / 100
	// waveformMaxPause is the longest pause in PCM that is still read
	// jitter; a longer pause, from a stall or an outage, is a gap.
	waveformMaxPause = 500 * time.Millisecond
	// A point holds min and max of the left channel, min and max of the right
	// channel, and a flags byte.
	waveformPointSize = 5
)

// Waveform point flags: a sample in the point reached ±ClipThreshold, or the
// point had no audio.
const (
	clipLeft byte = 1 << iota
	clipRight
	gapFlag
)

var gapPoint = [waveformPointSize]byte{4: gapFlag}

// Waveform keeps the min and max sample per channel per 10 ms of the last
// 10 seconds. Values are signed 8-bit: the 16-bit sample divided by 256, like
// audiowaveform -b 8. It is safe for concurrent use; a Write holds the lock
// throughout, so a Read never sees half of one.
type Waveform struct {
	mu sync.Mutex
	// The point being filled.
	frames                 int
	minL, maxL, minR, maxR int16
	flags                  byte

	ring   [WaveformPoints][waveformPointSize]byte
	end    int       // sequence number after the newest point
	lastAt time.Time // when audio last arrived
	clock  func() time.Time
}

// Write adds whole S16LE stereo frames; a point can span several writes.
func (w *Waveform) Write(buf []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Store the gap that Read has been reporting, so sequence numbers that
	// clients saw during the pause keep their meaning.
	now := w.now()
	if gap := w.pendingGap(now); gap > 0 {
		w.frames = 0 // The unfinished point is from before the pause.
		w.end += max(gap-WaveformPoints, 0)
		for range min(gap, WaveformPoints) {
			w.put(gapPoint)
		}
	}
	w.lastAt = now

	for i := 0; i+bytesPerFrame <= len(buf); i += bytesPerFrame {
		//nolint:gosec // Intentional reinterpretation of unsigned PCM to signed
		left := int16(binary.LittleEndian.Uint16(buf[i:]))
		//nolint:gosec // Intentional reinterpretation of unsigned PCM to signed
		right := int16(binary.LittleEndian.Uint16(buf[i+2:]))

		if w.frames == 0 {
			w.minL, w.maxL, w.minR, w.maxR, w.flags = left, left, right, right, 0
		} else {
			w.minL, w.maxL = min(w.minL, left), max(w.maxL, left)
			w.minR, w.maxR = min(w.minR, right), max(w.maxR, right)
		}
		if clipped(left) {
			w.flags |= clipLeft
		}
		if clipped(right) {
			w.flags |= clipRight
		}

		w.frames++
		if w.frames == waveformBucketFrames {
			w.frames = 0
			w.put([waveformPointSize]byte{to8(w.minL), to8(w.maxL), to8(w.minR), to8(w.maxR), w.flags})
		}
	}
}

// to8 divides with truncation toward zero, as audiowaveform does, instead of
// shifting, which would round negative values down.
func to8(v int16) byte {
	return byte(v / 256) //nolint:gosec // v/256 fits in int8; the byte keeps its two's complement.
}

// pendingGap is the number of 10 ms points since audio last arrived, once the
// pause exceeds waveformMaxPause. A read holds audio captured before it
// arrived, so a gap can run up to one read (100 ms) too long, more if audio
// arrives in a burst.
func (w *Waveform) pendingGap(now time.Time) int {
	pause := now.Sub(w.lastAt)
	if w.lastAt.IsZero() || pause <= waveformMaxPause {
		return 0
	}
	return int(pause / waveformBucket)
}

func (w *Waveform) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

func (w *Waveform) put(point [waveformPointSize]byte) {
	w.ring[w.end%WaveformPoints] = point
	w.end++
}

// Read returns the sequence number after the newest point and up to n of the
// newest points, oldest first, 5 bytes each; n must not exceed WaveformPoints.
// During a pause in the audio, the newest points are gap points up to now.
func (w *Waveform) Read(n int) (end int, points []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	end = w.end + w.pendingGap(w.now())
	points = make([]byte, 0, min(n, end)*waveformPointSize)
	for seq := max(end-n, 0); seq < end; seq++ {
		point := gapPoint
		if seq < w.end {
			point = w.ring[seq%WaveformPoints]
		}
		points = append(points, point[:]...)
	}
	return end, points
}
