package recording

import (
	"path/filepath"
	"testing"
)

func TestDiskSpaceOnRealFilesystem(t *testing.T) {
	t.Parallel()
	available, total, err := diskSpace(t.TempDir())
	if err != nil || total == 0 || available > total {
		t.Fatalf("diskSpace = %d, %d, %v", available, total, err)
	}
	if _, _, err := diskSpace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("diskSpace accepted a nonexistent path")
	}
}
