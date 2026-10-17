//go:build !linux

package recording

// clockSynchronized reports true: the encoder ships on Linux, where a Raspberry
// Pi without a real-time clock needs the check.
func clockSynchronized() bool { return true }
