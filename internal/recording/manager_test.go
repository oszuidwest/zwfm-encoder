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
		Codec:         types.CodecPCM,
		RecordingMode: types.RecordingOnDemand,
		StorageMode:   types.StorageLocal,
		LocalPath:     t.TempDir(),
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

func TestRecorderCodecMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		codec           types.Codec
		wantExtension   string
		wantContentType string
	}{
		{name: "mp3", codec: types.CodecMP3, wantExtension: "mp3", wantContentType: "audio/mpeg"},
		{name: "opus", codec: types.CodecOpus, wantExtension: "ts", wantContentType: "audio/mp2t"},
		{name: "pcm", codec: types.CodecPCM, wantExtension: "ts", wantContentType: "audio/mp2t"},
		{name: "unknown falls back to mpeg ts", codec: types.Codec("aac"), wantExtension: "ts", wantContentType: "audio/mp2t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := &GenericRecorder{
				id:     "recorder-test",
				config: types.Recorder{Codec: tt.codec},
			}
			if got := recorder.getFileExtension(); got != tt.wantExtension {
				t.Fatalf("getFileExtension() = %q, want %q", got, tt.wantExtension)
			}
			if got := recorder.getContentType(); got != tt.wantContentType {
				t.Fatalf("getContentType() = %q, want %q", got, tt.wantContentType)
			}
		})
	}
}
