package recording

import (
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"testing"
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
	rec.state = types.ProcessRotating // Covers rotateFile's transient state.
	rec.mu.Unlock()
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := rec.Status().State; got != types.ProcessStopped {
		t.Fatalf("rotating recorder left in %q after Manager.Stop; want stopped", got)
	}
}

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
		t.Fatal("Stop reassigned uploadQueue; it must be created once and reused for the recorder's lifetime")
	}
}

func TestQueueForUploadAfterWorkerStopPersistsToRetryQueue(t *testing.T) {
	spoolDir := t.TempDir()
	r, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: testS3Recorder(),
		SpoolDir: spoolDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.state = types.ProcessRunning
	r.uploadWorkerRunning = true
	r.mu.Unlock()
	r.uploadWg.Add(1)
	go r.uploadWorker()
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	filePath := writeSpoolFile(t, spoolDir, r.id, "late.mp3", "late-recording")
	r.queueForUpload(filePath)
	if got := len(r.uploadQueue); got != 0 {
		t.Fatalf("late upload stranded in channel with no worker: uploadQueue len = %d, want 0", got)
	}
	if got := len(r.retryQueue); got != 1 {
		t.Fatalf("late upload not persisted: retryQueue len = %d, want 1", got)
	}
}

func TestRecorderStatusReportsPendingUploads(t *testing.T) {
	t.Parallel()

	r, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: testS3Recorder(),
		SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.retryQueue = []pendingUpload{{}, {}}
	r.mu.Unlock()

	if got := r.Status().PendingUploads; got != 2 {
		t.Fatalf("PendingUploads = %d, want 2", got)
	}
}

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
