package silencedump

import (
	"bytes"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// refCopyFromRing is the byte-loop reference for copyFromRing.
func refCopyFromRing(buf, dst []byte, startPos int64) {
	capacity := int64(len(buf))
	bufferStart := startPos % capacity
	for i := range dst {
		pos := (bufferStart + int64(i)) % capacity
		dst[i] = buf[pos]
	}
}

// newPatternedCapturer returns a ring pattern that exposes wrap mistakes.
func newPatternedCapturer() *Capturer {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	for i := range c.buffer {
		c.buffer[i] = byte((i*131 + 7) & 0xff)
	}
	return c
}

// writePattern writes byte(p) at absolute position p for formula-based checks.
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

// TestCopyFromRingMatchesReference checks wrap boundaries and real snapshot sizes.
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

// TestWriteAudioMatchesByteLoopReference verifies writeToRing matches byte-loop
// behavior across mid-chunk wraps.
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

// TestOnSilenceStartSavedBeforeWrap verifies savedBefore keeps the newest
// pre-silence audio after the ring wraps.
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
