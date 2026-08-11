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

func testCaptureKey(trigger Trigger) captureKey {
	return captureKey{trigger: trigger}
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
		maxIncidentSeconds * audio.BytesPerSecond,
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
	c.OnIncidentStart(TriggerSilence, 0)
	state := c.captures[testCaptureKey(TriggerSilence)]
	if state == nil {
		t.Fatal("silence capture state missing")
	}
	wantLen := int(min(total, int64(beforeSeconds*audio.BytesPerSecond)))
	if len(state.savedBefore) != wantLen {
		t.Fatalf("savedBefore len: got %d want %d", len(state.savedBefore), wantLen)
	}
	start := total - int64(wantLen)
	for i := range state.savedBefore {
		want := byte((start + int64(i)) & 0xff)
		if state.savedBefore[i] != want {
			t.Fatalf("savedBefore[%d]=%d want %d (start=%d)", i, state.savedBefore[i], want, start)
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
				buffer:       make([]byte, bufferCapacity),
				enabled:      true,
				totalWritten: tt.totalWritten,
				captures: map[captureKey]*captureState{
					testCaptureKey(TriggerSilence): {startPos: tt.silenceStartPos},
				},
			}
			c.OnIncidentRecover(TriggerSilence, 0, 0, tt.recovery)
			state := c.captures[testCaptureKey(TriggerSilence)]
			if state.endPos != tt.wantEndPos {
				t.Fatalf("endPos = %d, want %d", state.endPos, tt.wantEndPos)
			}
			if state.endPos < state.startPos {
				t.Fatalf("endPos %d < startPos %d", state.endPos, state.startPos)
			}
		})
	}
}

func TestCheckAndFinalizeRecoversAtZeroStart(t *testing.T) {
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	c.OnIncidentStart(TriggerSilence, 0)                       // Keeps silenceStartPos at 0.
	c.OnIncidentRecover(TriggerSilence, 0, 0, 100*time.Second) // Clamps wall-clock recovery to bytes written.
	state := c.captures[testCaptureKey(TriggerSilence)]
	if !state.recovered {
		t.Fatal("recovered not set after OnIncidentRecover")
	}
	if state.endPos != 0 {
		t.Fatalf("endPos = %d, want 0 (clamped to start)", state.endPos)
	}
	writePattern(c, int64(afterSeconds*audio.BytesPerSecond))
	if c.captures[testCaptureKey(TriggerSilence)] != nil {
		t.Fatal("capturer stuck capturing; recovery at byte position 0 never finalized")
	}
}

func TestCapturerTracksSilenceAndImbalanceIndependently(t *testing.T) {
	t.Parallel()
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	writePattern(c, int64(audio.BytesPerSecond))

	c.OnIncidentStart(TriggerSilence, 0)
	writePattern(c, int64(audio.BytesPerSecond))
	c.OnIncidentStart(TriggerChannelImbalance, 0)
	c.OnIncidentRecover(TriggerSilence, 0, 2*time.Second, time.Second)
	c.OnIncidentRecover(TriggerChannelImbalance, 0, 3*time.Second, 500*time.Millisecond)

	silence := c.captures[testCaptureKey(TriggerSilence)]
	imbalance := c.captures[testCaptureKey(TriggerChannelImbalance)]
	if silence == nil || imbalance == nil {
		t.Fatalf("capture states missing: silence=%v imbalance=%v", silence != nil, imbalance != nil)
	}
	if silence == imbalance {
		t.Fatal("silence and imbalance share capture state")
	}
	if !silence.recovered || !imbalance.recovered {
		t.Fatalf("capture recovery state: silence=%v imbalance=%v", silence.recovered, imbalance.recovered)
	}
	if silence.startPos == imbalance.startPos {
		t.Fatalf("capture start positions both %d, want independent incident positions", silence.startPos)
	}
}

func TestSameTriggerReentryKeepsRecoveredCaptureUntilPostWindow(t *testing.T) {
	t.Parallel()
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	writePattern(c, int64(audio.BytesPerSecond))

	c.OnIncidentStart(TriggerSilence, 1)
	first := c.captures[captureKey{trigger: TriggerSilence, incidentID: 1}]
	c.OnIncidentRecover(TriggerSilence, 1, time.Second, 0)
	writePattern(c, int64(audio.BytesPerSecond))
	c.OnIncidentStart(TriggerSilence, 2)

	if first.savedBefore == nil {
		t.Fatal("first capture finalized before its post-recovery window was available")
	}
	if len(c.captures) != 2 {
		t.Fatalf("captures = %d, want both same-trigger incidents", len(c.captures))
	}

	writePattern(c, int64((afterSeconds-1)*audio.BytesPerSecond))
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 1}] != nil {
		t.Fatal("first capture still active after its full post-recovery window")
	}
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 2}] == nil {
		t.Fatal("second capture was removed while still active")
	}
}

func TestOnIncidentStartBoundsPerTriggerCaptures(t *testing.T) {
	t.Parallel()
	c := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	writePattern(c, int64(audio.BytesPerSecond))

	c.OnIncidentStart(TriggerSilence, 1)
	c.OnIncidentStart(TriggerSilence, 2)
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 1}] != nil {
		t.Fatal("abandoned un-recovered capture was not dropped")
	}
	if len(c.captures) != 1 {
		t.Fatalf("captures = %d, want only the superseding capture", len(c.captures))
	}

	c.OnIncidentRecover(TriggerSilence, 2, time.Second, 0)
	c.OnIncidentStart(TriggerSilence, 3)
	c.OnIncidentRecover(TriggerSilence, 3, time.Second, 0)
	c.OnIncidentStart(TriggerSilence, 4)
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 2}] != nil {
		t.Fatal("oldest recovered capture was not finalized on overflow")
	}
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 3}] == nil {
		t.Fatal("newer recovered capture was removed")
	}
	if c.captures[captureKey{trigger: TriggerSilence, incidentID: 4}] == nil {
		t.Fatal("new capture was not registered")
	}
}

func TestManagerHandlesChannelImbalanceEvents(t *testing.T) {
	t.Parallel()
	capturer := &Capturer{buffer: make([]byte, bufferCapacity), enabled: true}
	manager := &Manager{capturer: capturer}
	manager.HandleChannelImbalanceEvent(&audio.ImbalanceEvent{JustEntered: true})
	key := testCaptureKey(TriggerChannelImbalance)
	if capturer.captures[key] == nil {
		t.Fatal("imbalance start did not create capture state")
	}

	manager.HandleChannelImbalanceEvent(&audio.ImbalanceEvent{
		JustRecovered:      true,
		TotalDurationMs:    10000,
		RecoveryDurationMs: 5000,
	})
	if !capturer.captures[key].recovered {
		t.Fatal("imbalance recovery did not update capture state")
	}
}

func TestDumpFilenameIdentifiesChannelImbalance(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 8, 11, 20, 18, 26, 0, time.Local)
	tests := []struct {
		name    string
		trigger Trigger
		want    string
	}{
		{name: "silence keeps legacy filename", trigger: TriggerSilence, want: "2026-08-11_20-18-26.mp3"},
		{
			name:    "imbalance has source prefix",
			trigger: TriggerChannelImbalance,
			want:    "channel-imbalance-2026-08-11_20-18-26.mp3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dumpFilename(tt.trigger, startedAt); got != tt.want {
				t.Fatalf("dumpFilename(%q) = %q, want %q", tt.trigger, got, tt.want)
			}
		})
	}
}
