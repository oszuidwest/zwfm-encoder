package audio

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// framesPCM returns n S16LE stereo frames of left/right.
func framesPCM(n int, left, right int16) []byte {
	return makeKnownStereoPCM(n, func(int) (int16, int16) { return left, right })
}

func TestWaveformPointSpansWritesAndTruncates(t *testing.T) {
	t.Parallel()
	// The fake clock keeps a slow test machine from turning a write into a gap.
	synctest.Test(t, func(t *testing.T) {
		var w Waveform
		w.Write(framesPCM(100, -255, 300))
		w.Write(framesPCM(100, ClipThreshold, -ClipThreshold+1))
		w.Write(framesPCM(waveformBucketFrames-200, 0, 0))
		w.Write(framesPCM(waveformBucketFrames, 0, 0)) // Starts clean.

		end, points := w.Read(0)
		// Division truncates toward zero: -255 gives 0 and -32759 gives -127,
		// where a shift would give -1 and -128. Only the left channel clipped.
		want := []byte{0, 127, 0x81, 1, clipLeft, 0, 0, 0, 0, 0}
		if end != 2 || !bytes.Equal(points, want) {
			t.Fatalf("Read = %d, %v; want 2, %v", end, points, want)
		}
	})
}

func TestWaveformPauseReadsAsGapAndKeepsSequenceNumbers(t *testing.T) {
	t.Parallel()
	// The fake clock makes the pauses between writes exact.
	synctest.Test(t, func(t *testing.T) {
		var w Waveform
		point := func(v byte) []byte { return []byte{v, v, v, v, 0} }
		gap := []byte{0, 0, 0, 0, gapFlag}

		w.Write(framesPCM(waveformBucketFrames, 512, 512))
		w.Write(framesPCM(waveformBucketFrames/2, 32767, 32767)) // Unfinished; dropped at the pause.
		time.Sleep(530 * time.Millisecond)
		want := bytes.Join([][]byte{point(2), bytes.Repeat(gap, 53)}, nil)
		if end, points := w.Read(0); end != 54 || !bytes.Equal(points, want) {
			t.Fatalf("Read during pause = %d, %v; want 54, %v", end, points, want)
		}

		// The returning point follows the gap clients already saw.
		w.Write(framesPCM(waveformBucketFrames, -512, -512))
		// Audio that keeps arriving within the limit is no gap, even when a
		// point takes longer than that to fill.
		time.Sleep(400 * time.Millisecond)
		w.Write(framesPCM(waveformBucketFrames/2, 512, 512))
		time.Sleep(400 * time.Millisecond)
		w.Write(framesPCM(waveformBucketFrames/2, 512, 512))
		want = bytes.Join([][]byte{want, point(0xfe), point(2)}, nil)
		if end, points := w.Read(0); end != 56 || !bytes.Equal(points, want) {
			t.Fatalf("Read after pause = %d, %v; want 56, %v", end, points, want)
		}

		// An outage longer than the ring still advances by its full length; the
		// window then holds gap points up to the newest point, across the ring
		// end.
		const outage = WaveformPoints + 100
		time.Sleep(outage * waveformBucket)
		w.Write(framesPCM(waveformBucketFrames, 256, 256))
		want = bytes.Join([][]byte{bytes.Repeat(gap, WaveformPoints-1), point(1)}, nil)
		if end, points := w.Read(0); end != 56+outage+1 || !bytes.Equal(points, want) {
			t.Fatalf("Read after long outage = %d, %d bytes; want %d, %d bytes", end, len(points), 56+outage+1, len(want))
		}
	})
}

func TestWaveformReadFromReturnsEveryPointSince(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var w Waveform
		w.Write(framesPCM(5*waveformBucketFrames, 0, 0))
		from, _ := w.Read(0)
		w.Write(framesPCM(45*waveformBucketFrames, 0, 0)) // A burst after a short stall.
		if end, points := w.Read(from); end != 50 || len(points) != 45*waveformPointSize {
			t.Fatalf("Read(%d) = %d, %d points; want 50, 45", from, end, len(points)/waveformPointSize)
		}
		if _, points := w.Read(-1); len(points) != 50*waveformPointSize {
			t.Fatalf("Read(-1) = %d points; want all 50", len(points)/waveformPointSize)
		}
		if _, points := w.Read(51); len(points) != 0 {
			t.Fatalf("Read(51) = %d points; want none", len(points)/waveformPointSize)
		}
	})
}
