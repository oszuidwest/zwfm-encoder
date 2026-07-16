package util

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatedPath returns the rollover destination a RollingWriter uses for path:
// path plus a ".1" suffix. Readers that want to include rotated-out content
// derive the location from this function rather than hardcoding the suffix.
func RotatedPath(path string) string {
	return path + ".1"
}

// RollingWriter is a small size-capped writer: when a write would push the
// active file past maxSize bytes, the file is first renamed to
// [RotatedPath](path) (replacing any previous rollover) and a fresh file is
// opened, so a record is never split across generations. A single record
// larger than the cap, or a rotation whose rename is blocked, can leave the
// active file over the cap until a later write rotates it out. Concurrent
// Writes are serialized so the rotate check is atomic relative to writes.
// Rotation, reopen, and write failures are tolerated: the writer marks the
// file broken and retries the open on the next Write, so a transient lock
// (antivirus, indexer) can never silently stop logging for good.
//
// Rotation closes the file before renaming it: Go's os.OpenFile on Windows
// opens without FILE_SHARE_DELETE, so renaming a file this process holds
// open fails with a sharing violation.
type RollingWriter struct {
	mu           sync.Mutex
	path         string
	maxSize      int64
	closed       bool     // set by Close; Write fails with os.ErrClosed
	file         *os.File // nil when broken (failed open, rotate, or write); Write retries
	written      int64    // bytes since the last rotation attempt
	pendingTrunc int64    // -1 when none; else the record boundary a torn write must be rolled back to before the next append
}

// NewRollingWriter opens (creating if needed) the log file at path for
// appending, rotating whenever it would grow past maxSize bytes. maxSize
// must be positive.
func NewRollingWriter(path string, maxSize int64) (*RollingWriter, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("rolling writer: maxSize must be positive, got %d", maxSize)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // Log directory needs to be readable
		return nil, err
	}
	w := &RollingWriter{path: path, maxSize: maxSize, pendingTrunc: -1}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends p to the active file, rotating first when p would push it
// past the size cap. An empty active file is never rotated, so a single
// record larger than the cap still lands (and rotates out on the next
// write) without destroying the previous rollover. After Close, Write
// fails with [os.ErrClosed].
func (w *RollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file == nil {
		// A previous rotate, open, or write failed; retry before giving up
		// on this record.
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}

	if w.pendingTrunc >= 0 {
		// A torn record from an earlier partial write is still on disk.
		// Roll it back before appending anything, or refuse the record:
		// gluing a new record to the fragment would make both unreadable.
		if w.written > w.pendingTrunc {
			if err := w.file.Truncate(w.pendingTrunc); err != nil {
				_ = w.file.Close()
				w.file = nil
				return 0, err
			}
			w.written = w.pendingTrunc
		}
		w.pendingTrunc = -1
	}

	if w.written > 0 && w.written+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	if err != nil {
		// The handle may be unusable; drop it so the next Write reopens
		// fresh. A short write leaves a torn record on disk: roll it back
		// now, or remember the boundary so no later record is appended to
		// the fragment.
		if n > 0 {
			if size, ok := w.sizeLocked(); ok {
				boundary := size - int64(n)
				if w.file.Truncate(boundary) != nil {
					w.pendingTrunc = boundary
				}
			}
		}
		_ = w.file.Close()
		w.file = nil
		return n, err
	}
	w.written += int64(n)
	return n, nil
}

// sizeLocked reports the active file's current size, preferring the open
// handle and falling back to the path.
func (w *RollingWriter) sizeLocked() (int64, bool) {
	if info, err := w.file.Stat(); err == nil {
		return info.Size(), true
	}
	if info, err := os.Stat(w.path); err == nil {
		return info.Size(), true
	}
	return 0, false
}

// Close releases the file handle and marks the writer closed: subsequent
// Writes fail with [os.ErrClosed]. Close is idempotent.
func (w *RollingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.closed = true
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
// one, returning an error when the close or reopen fails (file stays nil and
// the next Write retries). A close error can mean delayed writes were lost,
// so it surfaces instead of rotating a possibly incomplete generation over
// the previous rollover. A rename failure is tolerated: losing rotation is
// preferable to losing logs.
func (w *RollingWriter) rotateLocked() error {
	closeErr := w.file.Close()
	w.file = nil
	if closeErr != nil {
		return closeErr
	}

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
