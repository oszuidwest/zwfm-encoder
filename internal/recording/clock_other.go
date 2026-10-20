//go:build !linux

package recording

// clockSynchronized skips kernel clock checks outside Linux.
func clockSynchronized() bool { return true }
