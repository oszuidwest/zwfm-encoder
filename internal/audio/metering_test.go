package audio

import (
	"fmt"
	"testing"
)

// makeStereoPCM builds deterministic S16LE stereo PCM of the given frame count.
// Each byte is a distinct step of a stride-7 ramp, so every position differs from
// its neighbours and any frame misalignment - a byte drift or an L/R swap -
// changes the decoded samples and is observable in the accumulated levels.
func makeStereoPCM(frames int) []byte {
	buf := make([]byte, frames*bytesPerFrame)
	var b byte = 1
	for i := range buf {
		buf[i] = b
		b += 7 // wraps mod 256; period 256 since gcd(7,256)==1
	}
	return buf
}

// feedInChunks streams pcm into data using the repeating sequence of chunk sizes,
// mimicking pipe reads that do not respect 4-byte frame boundaries.
func feedInChunks(data *LevelData, pcm []byte, sizes []int) {
	pos, si := 0, 0
	for pos < len(pcm) {
		end := pos + sizes[si%len(sizes)]
		si++
		if end > len(pcm) {
			end = len(pcm)
		}
		ProcessSamples(pcm[pos:end], end-pos, data)
		pos = end
	}
}

// TestProcessSamplesChunkingInvariance is the regression test for issue #296:
// the accumulated levels must not depend on where the byte stream is split. Pipe
// reads can land off frame boundaries; before the remainder carry, those splits
// dropped bytes and drifted the L/R alignment, producing garbage RMS/peak/clip
// values. The aligned single-call result is the reference every chunking must
// reproduce exactly (identical frames in identical order give identical floats).
func TestProcessSamplesChunkingInvariance(t *testing.T) {
	const frames = 5000
	pcm := makeStereoPCM(frames)

	var ref LevelData
	ProcessSamples(pcm, len(pcm), &ref)
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

// TestProcessSamplesSplitFrameClipCounting exercises the split-frame
// reconstruction path for the clip counter specifically. The ramp data in
// TestProcessSamplesChunkingInvariance never reaches ClipThreshold, so a frame
// reassembled from the wrong bytes could miscount clips and nothing would fail.
// Each case clips exactly one channel, so a byte drift or L/R swap in the split
// reassembly flips the clip attribution; every split point must decode
// identically to the aligned read.
func TestProcessSamplesSplitFrameClipCounting(t *testing.T) {
	cases := []struct {
		name      string
		frame     []byte
		wantClipL int
		wantClipR int
	}{
		// left = 32767 (0x7FFF) clips; right = 100 (0x0064) does not.
		{"left clips", []byte{0xFF, 0x7F, 0x64, 0x00}, 1, 0},
		// right = -32768 (0x8000) clips; left = 100 (0x0064) does not.
		{"right clips", []byte{0x64, 0x00, 0x00, 0x80}, 0, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var aligned LevelData
			ProcessSamples(c.frame, len(c.frame), &aligned)
			want := CalculateLevels(&aligned)
			if want.ClipLeft != c.wantClipL || want.ClipRight != c.wantClipR {
				t.Fatalf("setup: aligned clip counts L=%d R=%d, want L=%d R=%d",
					want.ClipLeft, want.ClipRight, c.wantClipL, c.wantClipR)
			}

			for split := 1; split <= 3; split++ {
				t.Run(fmt.Sprintf("split %d+%d", split, len(c.frame)-split), func(t *testing.T) {
					var d LevelData
					ProcessSamples(c.frame[:split], split, &d)
					ProcessSamples(c.frame[split:], len(c.frame)-split, &d)

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

// TestProcessSamplesCarriesTrailingPartialFrame asserts the end-of-stream carry:
// a buffer that ends mid-frame accumulates the whole frames and holds the
// trailing bytes for the next read.
func TestProcessSamplesCarriesTrailingPartialFrame(t *testing.T) {
	buf := makeStereoPCM(6)[:22] // 5 full frames (20 bytes) + 2 trailing bytes

	var d LevelData
	ProcessSamples(buf, len(buf), &d)

	if d.SampleCount != 5 {
		t.Fatalf("expected 5 whole frames, got %d", d.SampleCount)
	}
	if d.remainderLen != 2 {
		t.Fatalf("expected 2 carried bytes, got %d", d.remainderLen)
	}
}

// TestResetPreservesRemainder verifies that the periodic metering-window Reset
// keeps the partial-frame carry. Reset fires roughly every 250ms, far more often
// than reads split a frame, so dropping the carry on Reset would re-introduce the
// misalignment the fix removes.
func TestResetPreservesRemainder(t *testing.T) {
	pcm := makeStereoPCM(3) // 12 bytes = 3 frames

	var d LevelData
	ProcessSamples(pcm[:10], 10, &d) // 2 full frames + 2 trailing bytes
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

	ProcessSamples(pcm[10:], 2, &d) // final 2 bytes complete the third frame
	if d.SampleCount != 1 {
		t.Fatalf("expected the carried frame to complete, got %d", d.SampleCount)
	}
	if d.remainderLen != 0 {
		t.Fatalf("expected no leftover after completing the frame, got %d", d.remainderLen)
	}

	var ref LevelData
	ProcessSamples(pcm[8:12], 4, &ref) // frame 2 decoded in one aligned read
	if got, want := CalculateLevels(&d), CalculateLevels(&ref); got != want {
		t.Errorf("carried frame decoded incorrectly:\n got  %+v\n want %+v", got, want)
	}
}

// TestProcessSamplesEmptyBufferKeepsRemainder guards the n==0 edge: a zero-length
// read must not disturb a pending carry.
func TestProcessSamplesEmptyBufferKeepsRemainder(t *testing.T) {
	pcm := makeStereoPCM(1)

	var d LevelData
	ProcessSamples(pcm[:2], 2, &d) // 2 bytes carried, no frame yet
	if d.SampleCount != 0 || d.remainderLen != 2 {
		t.Fatalf("setup failed: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}

	ProcessSamples(nil, 0, &d)
	if d.SampleCount != 0 || d.remainderLen != 2 {
		t.Fatalf("empty read disturbed state: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}

	ProcessSamples(pcm[2:], 2, &d) // completes the frame
	if d.SampleCount != 1 || d.remainderLen != 0 {
		t.Fatalf("frame did not complete: SampleCount=%d remainderLen=%d", d.SampleCount, d.remainderLen)
	}
}
