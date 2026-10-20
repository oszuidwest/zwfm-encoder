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
	// f_bavail excludes blocks reserved for privileged users.
	blockSize := storageBlockSize(&stat)
	if blockSize == 0 || stat.Blocks == 0 || stat.Blocks > math.MaxUint64/blockSize {
		return 0, 0, fmt.Errorf("invalid filesystem block counts")
	}
	blocks := stat.Blocks
	free := stat.Bavail
	if free > blocks {
		free = 0 // Some filesystems report negative availability in this unsigned field.
	}
	return free * blockSize, blocks * blockSize, nil
}
