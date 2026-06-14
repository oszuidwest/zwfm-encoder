package silencedump

import (
	"bytes"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// refCopyFromRing is the byte-by-byte reference the optimized copyFromRing
// replaced. Tests assert the fast version is bit-for-bit equivalent to it.
func refCopyFromRing(buf, dst []byte, startPos int64) {
	capacity := int64(len(buf))
	bufferStart := startPos % capacity
	for i := range dst {
		pos := (bufferStart + int64(i)) % capacity
		dst[i] = buf[pos]
	}
}

// newPatternedCapturer returns a Capturer whose ring buffer is filled with a
// deterministic, non-repeating-within-a-byte pattern so off-by-one and wrap
// errors surface as mismatches.
func newPatternedCapturer() *Capturer {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	for i := range c.buffer {
		c.buffer[i] = byte((i*131 + 7) & 0xff)
	}
	return c
}

// writePattern feeds total bytes through WriteAudio in chunks, where the byte at
// absolute position p is byte(p). This lets tests assert ring contents and
// snapshots by formula instead of keeping a full history.
func writePattern(c *Capturer, total int64) {
	const chunk = 19200
	var abs int64
	for abs < total {
		sz := int64(chunk)
		if abs+sz > total {
			sz = total - abs
		}
		pcm := make([]byte, sz)
		for j := range pcm {
			pcm[j] = byte((abs + int64(j)) & 0xff)
		}
		c.WriteAudio(pcm)
		abs += sz
	}
}

// TestCopyFromRingMatchesReference checks the optimized copy against the
// byte-loop reference across start positions and lengths, including the
// wrap-around boundary and the real caller sizes (15s before, 5s silence,
// 15s after).
func TestCopyFromRingMatchesReference(t *testing.T) {
	c := newPatternedCapturer()

	starts := []int64{
		0, 1, 7,
		bufferCapacity - 1, bufferCapacity, bufferCapacity + 1,
		3 * bufferCapacity,
		999983, 12345678,
	}
	lens := []int{
		0, 1, 3, 100,
		bufferCapacity - 1, bufferCapacity,
		beforeSeconds * audio.BytesPerSecond,
		maxSilenceSeconds * audio.BytesPerSecond,
		afterSeconds * audio.BytesPerSecond,
	}

	for _, start := range starts {
		for _, l := range lens {
			got := make([]byte, l)
			want := make([]byte, l)
			c.copyFromRing(got, start)
			refCopyFromRing(c.buffer, want, start)
			if !bytes.Equal(got, want) {
				t.Fatalf("copyFromRing mismatch at start=%d len=%d", start, l)
			}
		}
	}
}

// TestCopyFromRingLongerThanCapacity covers the defensive multi-wrap path: a
// destination longer than the ring still matches the modulo reference, even
// though no production caller requests more than 15s.
func TestCopyFromRingLongerThanCapacity(t *testing.T) {
	c := newPatternedCapturer()

	l := bufferCapacity + 1234
	got := make([]byte, l)
	want := make([]byte, l)
	c.copyFromRing(got, 5)
	refCopyFromRing(c.buffer, want, 5)
	if !bytes.Equal(got, want) {
		t.Fatal("copyFromRing mismatch for dst longer than bufferCapacity")
	}
}

// TestWriteAudioMatchesByteLoopReference drives WriteAudio (which now uses
// writeToRing) with chunks that cross the wrap boundary and compares the ring
// contents, write position, and total against the original per-byte write loop.
func TestWriteAudioMatchesByteLoopReference(t *testing.T) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}

	refBuf := make([]byte, bufferCapacity)
	refWritePos := 0
	var refTotal int64

	// Sizes chosen so the running total wraps the ring mid-chunk at least once.
	chunkSizes := []int{19200, 19200, 4096, 1, bufferCapacity / 2, bufferCapacity/2 + 5000, 19200, 7}
	var abs int64
	for _, sz := range chunkSizes {
		pcm := make([]byte, sz)
		for j := range pcm {
			pcm[j] = byte((abs + int64(j)) & 0xff)
		}
		abs += int64(sz)

		c.WriteAudio(pcm)

		for _, b := range pcm {
			refBuf[refWritePos] = b
			refWritePos = (refWritePos + 1) % bufferCapacity
		}
		refTotal += int64(sz)
	}

	if c.writePos != refWritePos {
		t.Fatalf("writePos mismatch: got %d want %d", c.writePos, refWritePos)
	}
	if c.totalWritten != refTotal {
		t.Fatalf("totalWritten mismatch: got %d want %d", c.totalWritten, refTotal)
	}
	if !bytes.Equal(c.buffer, refBuf) {
		t.Fatal("ring buffer contents mismatch vs byte-loop reference")
	}
}

// TestOnSilenceStartSavedBeforeWrap exercises copyFromRing through its real
// caller. Writing more than one ring's worth forces the savedBefore start
// position to wrap, and the byte(p)-at-position-p pattern lets us assert the
// snapshot is exactly the most recent 15s by formula.
func TestOnSilenceStartSavedBeforeWrap(t *testing.T) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}

	total := int64(bufferCapacity) + 500_000
	writePattern(c, total)

	c.OnSilenceStart()

	wantLen := int(min(total, int64(beforeSeconds*audio.BytesPerSecond)))
	if len(c.savedBefore) != wantLen {
		t.Fatalf("savedBefore len: got %d want %d", len(c.savedBefore), wantLen)
	}

	start := total - int64(wantLen)
	for i := range c.savedBefore {
		want := byte((start + int64(i)) & 0xff)
		if c.savedBefore[i] != want {
			t.Fatalf("savedBefore[%d]=%d want %d (start=%d)", i, c.savedBefore[i], want, start)
		}
	}
}

func BenchmarkCopyFromRing(b *testing.B) {
	c := newPatternedCapturer()
	dst := make([]byte, afterSeconds*audio.BytesPerSecond) // 15s, a real extract size
	b.SetBytes(int64(len(dst)))
	for i := 0; b.Loop(); i++ {
		c.copyFromRing(dst, int64(i)*999983+7)
	}
}

func BenchmarkWriteToRing(b *testing.B) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	src := make([]byte, audio.BytesPerSecond/10) // ~100ms distributor chunk
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		c.writePos = c.writeToRing(src)
	}
}
