package util

import (
	"os"
	"path/filepath"
	"sync"
)

// rollingWriter is a small size-capped writer: when the active file reaches
// maxSize bytes, it is renamed to path+".1" (replacing any previous rollover)
// and a fresh file is opened. Concurrent Writes are serialized so the rotate
// check is atomic relative to writes. Rotation and reopen failures are
// tolerated: the writer marks the file broken and retries the open on the
// next Write, so a transient lock (antivirus, indexer) can never silently
// stop logging for good.
//
// Rotation closes the file before renaming it: Go's os.OpenFile on Windows
// opens without FILE_SHARE_DELETE, so renaming a file this process holds
// open fails with a sharing violation.
type rollingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File // nil when the last (re)open failed; Write retries
	written int64    // bytes since the last rotation attempt
}

func newRollingWriter(path string, maxSize int64) (*rollingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // Log directory needs to be readable
		return nil, err
	}
	w := &rollingWriter{path: path, maxSize: maxSize}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		// A previous rotate or open failed; retry before giving up on this
		// record.
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}

	if w.written+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

// openLocked opens the log file for appending and resumes the byte count
// from its current size. On failure the writer stays marked broken
// (file == nil).
func (w *rollingWriter) openLocked() error {
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

// rotateLocked renames the active file to path+".1" and opens a fresh one,
// returning an error only when the reopen fails (file stays nil and the next
// Write retries). The old handle is discarded even if Close errors - a *File
// is unusable after Close regardless - so this never leaves a dead handle
// behind. A rename failure is tolerated: losing rotation is preferable to
// losing logs.
func (w *rollingWriter) rotateLocked() error {
	_ = w.file.Close()
	w.file = nil

	rotated := w.path + ".1"
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
