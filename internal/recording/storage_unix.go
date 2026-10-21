//go:build linux || darwin

package recording

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func diskSpace(path string) (available, total uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return statfsSpace(stat.Blocks, stat.Bfree, stat.Bavail, storageBlockSize(&stat))
}

// statfsSpace returns the space available to the process and, as total, used
// plus available blocks. Like the caller-available counts on Windows, that
// total leaves out free blocks reserved for privileged users, so freeing space
// can bring available up to the total (less what the filesystem itself keeps
// allocated).
func statfsSpace(blocks, bfree, bavail, blockSize uint64) (available, total uint64, err error) {
	if blockSize == 0 || blocks == 0 || blocks > math.MaxUint64/blockSize {
		return 0, 0, fmt.Errorf("invalid filesystem block counts")
	}
	free := bavail
	if free > blocks {
		free = 0 // Some filesystems report negative availability in this unsigned field.
	}
	used := blocks - min(bfree, blocks)
	return free * blockSize, (used + free) * blockSize, nil
}
