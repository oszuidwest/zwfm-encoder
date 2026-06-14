package recording

import (
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// TestWriteAudioNeverBlocksAndCountsDrops verifies overflow drops newest chunks and counts them.
func TestWriteAudioNeverBlocksAndCountsDrops(t *testing.T) {
	t.Parallel()

	// Buffer of 2 with no writer draining it: the third chunk onwards overflow.
	r := &GenericRecorder{id: "r1", state: types.ProcessRunning, audioCh: make(chan []byte, 2)}

	done := make(chan struct{})
	go func() {
		for i := range 5 {
			r.WriteAudio([]byte{byte(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WriteAudio blocked when the buffer was full")
	}

	if got := r.audioDrops.Load(); got != 3 {
		t.Fatalf("audioDrops = %d, want 3", got)
	}
	if got := len(r.audioCh); got != 2 {
		t.Fatalf("buffered chunks = %d, want 2", got)
	}

	// Drop-newest keeps the two oldest chunks (0 and 1), drops 2, 3, 4.
	if first := <-r.audioCh; first[0] != 0 {
		t.Fatalf("first buffered chunk = %d, want 0", first[0])
	}
	if second := <-r.audioCh; second[0] != 1 {
		t.Fatalf("second buffered chunk = %d, want 1", second[0])
	}
}

// TestWriteAudioConcurrentWithTeardownNoPanic pins the send-vs-close race.
func TestWriteAudioConcurrentWithTeardownNoPanic(t *testing.T) {
	for range 50 {
		r := &GenericRecorder{id: "r1", state: types.ProcessRunning, audioCh: make(chan []byte, 4)}

		// Drain the channel so WriteAudio mostly hits the successful send path.
		ch := r.audioCh
		drained := make(chan struct{})
		go func() {
			for range ch {
			}
			close(drained)
		}()

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 200 {
					r.WriteAudio([]byte{1, 2, 3, 4})
				}
			}()
		}

		// Mirror stopEncoderAndUpload: clear under lock, close outside it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.mu.Lock()
			audioCh := r.audioCh
			r.audioCh = nil
			r.mu.Unlock()
			if audioCh != nil {
				close(audioCh)
			}
		}()

		wg.Wait()
		<-drained
	}
}

// TestWriteAudioNoopWhenNotRecording verifies missing or inactive writers never enqueue.
func TestWriteAudioNoopWhenNotRecording(t *testing.T) {
	t.Parallel()

	// Running but no channel (before startEncoderLocked): must not panic/drop.
	noCh := &GenericRecorder{id: "r1", state: types.ProcessRunning}
	noCh.WriteAudio([]byte{1})
	if got := noCh.audioDrops.Load(); got != 0 {
		t.Fatalf("audioDrops with nil channel = %d, want 0", got)
	}

	// Stopped recorder with a channel: must not enqueue.
	stopped := &GenericRecorder{id: "r2", state: types.ProcessStopped, audioCh: make(chan []byte, 2)}
	stopped.WriteAudio([]byte{1})
	if got := len(stopped.audioCh); got != 0 {
		t.Fatalf("buffered chunks while stopped = %d, want 0", got)
	}
}

// TestWriteAudioEnqueuesWhileRotating verifies boundary audio reaches a live rotating writer.
func TestWriteAudioEnqueuesWhileRotating(t *testing.T) {
	t.Parallel()

	r := &GenericRecorder{id: "r1", state: types.ProcessRotating, audioCh: make(chan []byte, 2)}
	r.WriteAudio([]byte{1})
	if got := len(r.audioCh); got != 1 {
		t.Fatalf("rotating recorder with a live channel buffered %d chunks, want 1", got)
	}

	// Once teardown has nil'd the channel, a rotating recorder is skipped.
	r.audioCh = nil
	r.WriteAudio([]byte{2})
	if got := r.audioDrops.Load(); got != 0 {
		t.Fatalf("audioDrops after nil-channel rotating write = %d, want 0", got)
	}
}

// TestStatusReportsAudioDrops verifies dropped chunks surface in recorder status.
func TestStatusReportsAudioDrops(t *testing.T) {
	t.Parallel()

	r := &GenericRecorder{id: "r1", state: types.ProcessRunning}
	r.audioDrops.Store(7)
	if got := r.Status().AudioDrops; got != 7 {
		t.Fatalf("Status().AudioDrops = %d, want 7", got)
	}
}

// TestManagerWriteAudioSharesOneCopyAcrossRecorders verifies active recorders share one copy.
func TestManagerWriteAudioSharesOneCopyAcrossRecorders(t *testing.T) {
	t.Parallel()

	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}

	add := func(id string, state types.ProcessState) *GenericRecorder {
		cfg := &types.Recorder{
			ID:            id,
			Name:          id,
			Codec:         types.CodecPCM,
			RecordingMode: types.RecordingOnDemand,
			StorageMode:   types.StorageLocal,
			LocalPath:     t.TempDir(),
		}
		if err := m.AddRecorder(cfg); err != nil {
			t.Fatalf("add recorder %s: %v", id, err)
		}
		rec := m.recorders[id]
		rec.state = state
		rec.audioCh = make(chan []byte, 4)
		return rec
	}

	a := add("a", types.ProcessRunning)
	b := add("b", types.ProcessRunning)
	c := add("c", types.ProcessStopped)

	// Distributor passes a reusable buffer; the manager must copy it.
	src := []byte{1, 2, 3, 4}
	m.WriteAudio(src)
	src[0] = 99 // Simulate distributor buffer reuse.

	ga := <-a.audioCh
	gb := <-b.audioCh
	if ga[0] != 1 || gb[0] != 1 {
		t.Fatalf("recorders saw the mutated source buffer: a=%d b=%d, want 1", ga[0], gb[0])
	}
	// Both recorders share the single copied slice (copy-once optimization).
	if &ga[0] != &gb[0] {
		t.Fatal("expected active recorders to share one copied slice")
	}
	if got := len(c.audioCh); got != 0 {
		t.Fatalf("inactive recorder received %d chunks, want 0", got)
	}
}
