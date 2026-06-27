package silencedump

import (
	"bytes"
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"testing"
	"time"
)

func refCopyFromRing(buf, dst []byte, startPos int64) {
	capacity := int64(len(buf))
	bufferStart := startPos % capacity
	for i := range dst {
		pos := (bufferStart + int64(i)) % capacity
		dst[i] = buf[pos]
	}
}

func newPatternedCapturer() *Capturer {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	for i := range c.buffer {
		c.buffer[i] = byte((i*131 + 7) & 0xff)
	}
	return c
}

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

func TestWriteAudioMatchesByteLoopReference(t *testing.T) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	refBuf := make([]byte, bufferCapacity)
	refWritePos := 0
	var refTotal int64
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

func TestOnSilenceRecoverClampsEndPos(t *testing.T) {
	const sec = int64(audio.BytesPerSecond)
	tests := []struct {
		name            string
		silenceStartPos int64
		totalWritten    int64
		recovery        time.Duration
		wantEndPos      int64
	}{
		{
			name:            "normal recovery backdates within range",
			silenceStartPos: 1 * sec,
			totalWritten:    10 * sec,
			recovery:        2 * time.Second,
			wantEndPos:      8 * sec, // 10s minus 2s.
		},
		{
			name:            "recovery exceeds bytes written clamps to start",
			silenceStartPos: 1 * sec,
			totalWritten:    2 * sec,
			recovery:        100 * time.Second, // Would yield a negative position.
			wantEndPos:      1 * sec,
		},
		{
			name:            "recovery reaching past start clamps to start",
			silenceStartPos: 5 * sec,
			totalWritten:    6 * sec,
			recovery:        3 * time.Second, // 6s minus 3s is before startPos.
			wantEndPos:      5 * sec,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Capturer{
				buffer:          make([]byte, bufferCapacity),
				enabled:         true,
				capturing:       true,
				silenceStartPos: tt.silenceStartPos,
				totalWritten:    tt.totalWritten,
			}
			c.OnSilenceRecover(0, tt.recovery)
			if c.silenceEndPos != tt.wantEndPos {
				t.Fatalf("silenceEndPos = %d, want %d", c.silenceEndPos, tt.wantEndPos)
			}
			if c.silenceEndPos < c.silenceStartPos {
				t.Fatalf("silenceEndPos %d < silenceStartPos %d", c.silenceEndPos, c.silenceStartPos)
			}
		})
	}
}

func TestCheckAndFinalizeRecoversAtZeroStart(t *testing.T) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	c.OnSilenceStart()                     // Keeps silenceStartPos at 0.
	c.OnSilenceRecover(0, 100*time.Second) // Clamps wall-clock recovery to bytes written.
	if !c.recovered {
		t.Fatal("recovered not set after OnSilenceRecover")
	}
	if c.silenceEndPos != 0 {
		t.Fatalf("silenceEndPos = %d, want 0 (clamped to start)", c.silenceEndPos)
	}
	writePattern(c, int64(afterSeconds*audio.BytesPerSecond))
	if c.capturing {
		t.Fatal("capturer stuck capturing; recovery at byte position 0 never finalized")
	}
}
