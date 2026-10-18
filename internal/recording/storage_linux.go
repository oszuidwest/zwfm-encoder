package recording

import "golang.org/x/sys/unix"

func storageBlockSize(stat *unix.Statfs_t) uint64 {
	// On Linux counts use fragment units, not the optimal transfer size.
	// Kernels/filesystems without f_frsize fall back to f_bsize.
	if stat.Frsize > 0 {
		return uint64(stat.Frsize)
	}
	if stat.Bsize > 0 {
		return uint64(stat.Bsize)
	}
	return 0
}
