package recording

import (
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"testing"
)

func TestSetMaxDurationMinutesPropagatesToExistingRecorders(t *testing.T) {
	m, _ := newTestManager(t, localOnDemandRecorder(t, "r1"))
	maxMinutes := func() (minutes int) {
		m.recorders["r1"].inspect(func(l *run) { minutes = l.maxMinutes })
		return minutes
	}
	if got := maxMinutes(); got != 60 {
		t.Fatalf("initial maxDurationMinutes: got %d, want 60", got)
	}
	m.SetMaxDurationMinutes(90)
	if got := maxMinutes(); got != 90 {
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
			if got := tt.codec.FileExtension(); got != tt.wantExtension {
				t.Fatalf("FileExtension() = %q, want %q", got, tt.wantExtension)
			}
			if got := tt.codec.ContentType(); got != tt.wantContentType {
				t.Fatalf("ContentType() = %q, want %q", got, tt.wantContentType)
			}
		})
	}
}
func testS3Recorder() *types.Recorder {
	return &types.Recorder{
		ID:                "r1",
		Name:              "Test",
		Enabled:           true,
		Codec:             types.CodecMP3,
		RecordingMode:     types.RecordingHourly,
		StorageMode:       types.StorageS3,
		S3Bucket:          "bucket",
		S3AccessKeyID:     "access",
		S3SecretAccessKey: "secret",
	}
}
