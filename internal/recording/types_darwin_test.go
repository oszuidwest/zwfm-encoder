package recording

import (
	"path/filepath"
	"testing"
)

func TestDefaultSpoolDirIsPerUser(t *testing.T) {
	// Keep this test serial: HOME is process-wide.
	home := t.TempDir()
	for _, tt := range []struct {
		name, home, want string
	}{
		{"home set", home, filepath.Join(home, "Library", "Application Support", "encoder", "recordings")},
		{"home unset", "", LinuxStateRecordingDir},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", tt.home)
			if got := DefaultSpoolDir(); got != tt.want {
				t.Fatalf("DefaultSpoolDir()=%q, want %q", got, tt.want)
			}
		})
	}
}
