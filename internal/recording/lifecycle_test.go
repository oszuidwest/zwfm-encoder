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
