package util

import (
	"os"
	"path/filepath"
	"sync"
)

// rotatedSuffix is appended to a log path to form its rollover destination.
const rotatedSuffix = ".1"

// RotatedPath returns the rollover destination a RollingWriter uses for path:
// path plus a ".1" suffix. Readers that want to include rotated-out content
// derive the location from this function rather than hardcoding the suffix.
func RotatedPath(path string) string {
	return path + rotatedSuffix
}

// RollingWriter is a small size-capped writer: when a write would push the
// active file past maxSize bytes, the file is first renamed to
// [RotatedPath](path) (replacing any previous rollover) and a fresh file is
// opened, so the active file never exceeds the cap and a record is never
// split across generations. Concurrent Writes are serialized so the rotate
// check is atomic relative to writes. Rotation and reopen failures are
// tolerated: the writer marks the file broken and retries the open on the
// next Write, so a transient lock (antivirus, indexer) can never silently
// stop logging for good.
//
// Rotation closes the file before renaming it: Go's os.OpenFile on Windows
// opens without FILE_SHARE_DELETE, so renaming a file this process holds
// open fails with a sharing violation.
type RollingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File // nil when the last (re)open failed; Write retries
	written int64    // bytes since the last rotation attempt
}

// NewRollingWriter opens (creating if needed) the log file at path for
// appending, rotating whenever it would grow past maxSize bytes. maxSize
// must be positive.
func NewRollingWriter(path string, maxSize int64) (*RollingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // Log directory needs to be readable
		return nil, err
	}
	w := &RollingWriter{path: path, maxSize: maxSize}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends p to the active file, rotating first when p would push it
// past the size cap. An empty active file is never rotated, so a single
// record larger than the cap still lands (and rotates out on the next
// write) without destroying the previous rollover.
func (w *RollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		// A previous rotate or open failed; retry before giving up on this
		// record.
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}

	if w.written > 0 && w.written+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

// Close releases the file handle. The writer is not dead afterwards: a
// subsequent Write reopens the file, the same self-healing path used after
// a failed rotation.
func (w *RollingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// openLocked opens the log file for appending and resumes the byte count
// from its current size. On failure the writer stays marked broken
// (file == nil).
func (w *RollingWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Log file needs to be readable
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.written = info.Size()
	return nil
}

// rotateLocked renames the active file to its rotated path and opens a fresh
// one, returning an error only when the reopen fails (file stays nil and the
// next Write retries). The old handle is discarded even if Close errors - a
// *File is unusable after Close regardless - so this never leaves a dead
// handle behind. A rename failure is tolerated: losing rotation is preferable
// to losing logs.
func (w *RollingWriter) rotateLocked() error {
	_ = w.file.Close()
	w.file = nil

	rotated := RotatedPath(w.path)
	_ = os.Remove(rotated)
	renameErr := os.Rename(w.path, rotated)

	if err := w.openLocked(); err != nil {
		return err
	}
	if renameErr != nil {
		// The reopened file still holds the old content and exceeds maxSize;
		// reset the counter so rotation is retried after another maxSize
		// bytes instead of on every write.
		w.written = 0
	}
	return nil
}
