package types

import (
	"strings"
	"testing"
)

func TestRecorderValidateRejectsUnsanitizedFilePrefix(t *testing.T) {
	t.Parallel()
	r := Recorder{Name: "rec", Codec: CodecPCM, RecordingMode: RecordingHourly, StorageMode: StorageLocal, LocalPath: "/tmp/rec", FilePrefix: "a/b"}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "file_prefix") {
		t.Fatalf("Validate() error = %v, want a file_prefix error", err)
	}
}
