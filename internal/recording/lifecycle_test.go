package recording

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func localOnDemandRecorder(t *testing.T, id string) *types.Recorder {
	t.Helper()
	return &types.Recorder{
		ID:            id,
		Name:          id,
		Codec:         types.CodecPCM,
		RecordingMode: types.RecordingOnDemand,
		StorageMode:   types.StorageLocal,
		LocalPath:     t.TempDir(),
	}
}

// TestManagerStopStopsRotatingRecorder is a regression test for finding 1:
// Manager.Stop must stop a recorder caught in a transitional state (here,
// ProcessRotating), not just one whose state is exactly ProcessRunning. A
// skipped rotating recorder would keep running and rotateFile could spin up a
// fresh FFmpeg after Stop returned.
func TestManagerStopStopsRotatingRecorder(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(localOnDemandRecorder(t, "r1")); err != nil {
		t.Fatal(err)
	}

	rec := m.recorders["r1"]
	rec.mu.Lock()
	rec.state = types.ProcessRotating // the state rotateFile holds during a rotation
	rec.mu.Unlock()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := rec.Status().State; got != types.ProcessStopped {
		t.Fatalf("rotating recorder left in %q after Manager.Stop; want stopped", got)
	}
}

// TestStopReusesUploadQueue is a regression test for finding 3: Stop must not
// reassign uploadQueue. queueForUpload reads that field without a lock, so a
// concurrent rotation racing Stop's reassignment is a data race and can drop an
// upload. The channel is created once and reused for the recorder's lifetime.
func TestStopReusesUploadQueue(t *testing.T) {
	r, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: testS3Recorder(),
		SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	before := r.uploadQueue

	r.mu.Lock()
	r.state = types.ProcessRunning
	r.mu.Unlock()

	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}

	if r.uploadQueue != before {
		t.Fatal("Stop reassigned uploadQueue; queueForUpload reads it without a lock, so this races a concurrent rotation and can drop an upload")
	}
}

// TestQueueForUploadAfterWorkerStopPersistsToRetryQueue is a regression test for
// the stop-vs-rotation upload stranding: once Stop has drained and exited the
// upload worker, a late queueForUpload (e.g. from a rotation that finalized
// after Stop claimed the result) must NOT land in the now-undrained in-memory
// channel. It must be persisted durably via the retry queue so it is recovered
// on restart for every storage mode.
func TestQueueForUploadAfterWorkerStopPersistsToRetryQueue(t *testing.T) {
	spoolDir := t.TempDir()
	r, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: testS3Recorder(),
		SpoolDir: spoolDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an active recorder with a running upload worker, then stop it.
	r.mu.Lock()
	r.state = types.ProcessRunning
	r.uploadWorkerRunning = true
	r.mu.Unlock()
	r.uploadWg.Add(1)
	go r.uploadWorker()

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A rotation that finished after Stop now tries to queue its file.
	filePath := writeSpoolFile(t, spoolDir, r.id, "late.mp3", "late-recording")
	r.queueForUpload(filePath)

	if got := len(r.uploadQueue); got != 0 {
		t.Fatalf("late upload stranded in channel with no worker: uploadQueue len = %d, want 0", got)
	}
	if got := len(r.retryQueue); got != 1 {
		t.Fatalf("late upload not persisted: retryQueue len = %d, want 1", got)
	}
}

// TestRemoveRecorderStopsAndRemoves verifies RemoveRecorder removes the recorder
// from the map and stops it (finding 2 fix keeps Stop outside the manager lock).
func TestRemoveRecorderStopsAndRemoves(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(localOnDemandRecorder(t, "r1")); err != nil {
		t.Fatal(err)
	}

	rec := m.recorders["r1"]
	rec.mu.Lock()
	rec.state = types.ProcessRunning
	rec.mu.Unlock()

	if err := m.RemoveRecorder("r1"); err != nil {
		t.Fatalf("RemoveRecorder: %v", err)
	}

	m.mu.RLock()
	_, exists := m.recorders["r1"]
	m.mu.RUnlock()
	if exists {
		t.Fatal("recorder still present in map after RemoveRecorder")
	}
	if got := rec.Status().State; got != types.ProcessStopped {
		t.Fatalf("removed recorder state = %q, want stopped", got)
	}
}
