package recording

import "golang.org/x/sys/unix"

func storageBlockSize(stat *unix.Statfs_t) uint64 {
	// Darwin's statfs counts use f_bsize; f_iosize is only the transfer size.
	return uint64(stat.Bsize)
}
