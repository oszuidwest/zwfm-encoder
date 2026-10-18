package recording

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestStorageBlockSizeUsesFragmentUnits(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		stat unix.Statfs_t
		want uint64
	}{
		{"fragment differs from transfer size", unix.Statfs_t{Bsize: 65536, Frsize: 4096}, 4096},
		{"missing fragment size", unix.Statfs_t{Bsize: 4096}, 4096},
		{"invalid block size", unix.Statfs_t{Bsize: -1}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := storageBlockSize(&tt.stat); got != tt.want {
				t.Fatalf("blockSize=%d, want %d", got, tt.want)
			}
		})
	}
}
