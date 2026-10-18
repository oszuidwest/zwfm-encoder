package recording

import "golang.org/x/sys/unix"

func storageBlockSize(stat *unix.Statfs_t) uint64 {
	// Counts use fragment units; the kernel fills f_frsize from f_bsize when a
	// filesystem leaves it unset.
	return uint64(max(stat.Frsize, 0))
}
