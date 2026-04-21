package recording

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestSetMaxDurationMinutesPropagatesToExistingRecorders(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &types.Recorder{
		ID:            "r1",
		Name:          "Test",
		RecordingMode: types.RecordingOnDemand,
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}

	// Recorder was constructed with the initial 60-minute limit.
	if got := m.recorders["r1"].maxDurationMinutes; got != 60 {
		t.Fatalf("initial maxDurationMinutes: got %d, want 60", got)
	}

	m.SetMaxDurationMinutes(90)

	// The existing recorder must reflect the new limit without needing a restart.
	if got := m.recorders["r1"].maxDurationMinutes; got != 90 {
		t.Errorf("after SetMaxDurationMinutes(90): got %d, want 90", got)
	}
}
