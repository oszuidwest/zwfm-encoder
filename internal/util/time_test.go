package util

import (
	"testing"
	"time"
)

func TestFilenameTimeParsesSuffixedRecordingName(t *testing.T) {
	t.Parallel()

	got, ok := FilenameTime("Studio-2026-09-28-14-37-2.mp3")
	if !ok {
		t.Fatal("FilenameTime() did not parse a collision-suffixed recording name")
	}
	want := time.Date(2026, time.September, 28, 14, 37, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("FilenameTime() = %v, want %v", got, want)
	}
}
