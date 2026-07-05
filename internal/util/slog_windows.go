//go:build windows

package util

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// slogLogCandidates returns fallback slog destinations, most preferred first.
// %PROGRAMDATA% may not be writable for a non-admin user (e.g. when an
// elevated first run created the file), so %LOCALAPPDATA% and the temp dir
// serve as user-writable fallbacks. The path is fixed rather than per-port
// because SetupLogging runs before config load; two encoder instances on one
// machine would share (and fight over) the first file. The event log at
// %PROGRAMDATA%\encoder\logs\<port>\encoder.jsonl remains the authoritative
// operational record; this file only captures slog output.
func slogLogCandidates() []string {
	var dirs []string
	if d := os.Getenv("PROGRAMDATA"); d != "" {
		dirs = append(dirs, d)
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		dirs = append(dirs, d)
	}
	dirs = append(dirs, os.TempDir())

	paths := make([]string, 0, len(dirs))
	for _, d := range dirs {
		paths = append(paths, filepath.Join(d, "encoder", "encoder.log"))
	}
	return paths
}

const slogMaxSizeBytes int64 = 5 * 1024 * 1024

// SetupLogging redirects slog to a file when stderr is not attached (Windows
// GUI build with -H windowsgui hides the console). When stderr is a live
// handle, this is a no-op and the default handler keeps writing there.
func SetupLogging() {
	if stderrAttached() {
		return
	}

	for _, path := range slogLogCandidates() {
		w, err := newRollingWriter(path, slogMaxSizeBytes)
		if err != nil {
			continue
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))
		return
	}
	// No stderr and no writable location at all - nothing more we can do.
}

// stderrAttached reports whether os.Stderr points at a real handle. On a
// Windows GUI build the standard handles are unset and Stat fails.
func stderrAttached() bool {
	_, err := os.Stderr.Stat()
	return err == nil
}

// rollingWriter is a small size-capped writer: when the active file reaches
// maxSize bytes, it is renamed to path+".1" (replacing any previous rollover)
// and a fresh file is opened. Concurrent Writes are serialized so the rotate
// check is atomic relative to writes. Rotation and reopen failures are
// tolerated: the writer marks the file broken and retries the open on the
// next Write, so a transient lock (antivirus, indexer) can never silently
// stop logging for good.
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
		// Best-effort: on rename failure the current file keeps growing and
		// rotation is retried after another maxSize bytes; on reopen failure
		// the next Write retries the open. Losing rotation is preferable to
		// losing logs.
		_ = w.rotateLocked()
		if w.file == nil {
			return 0, os.ErrClosed
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

// rotateLocked renames the active file to path+".1" and opens a fresh one.
// The old handle is discarded even if Close errors - a *File is unusable
// after Close regardless - so this never leaves a dead handle behind.
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
	return renameErr
}
