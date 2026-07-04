//go:build windows

package util

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// slogLogPath is the fallback slog destination when stderr is not attached
// (Windows GUI build). Kept fixed rather than per-port because SetupLogging
// runs before config load. The event log at
// %PROGRAMDATA%\encoder\logs\<port>\encoder.jsonl remains the authoritative
// operational record; this file only captures slog output.
func slogLogPath() string {
	base := os.Getenv("PROGRAMDATA")
	if base == "" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "encoder", "encoder.log")
}

const slogMaxSizeBytes int64 = 5 * 1024 * 1024

// SetupLogging redirects slog to a file when stderr is not attached (Windows
// GUI build with -H windowsgui hides the console). When stderr is a live
// handle, this is a no-op and the default handler keeps writing there.
func SetupLogging() {
	if stderrAttached() {
		return
	}

	w, err := newRollingWriter(slogLogPath(), slogMaxSizeBytes)
	if err != nil {
		// No stderr and no writable log file — nothing to do. Errors would
		// go nowhere anyway.
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
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
// check is atomic relative to writes.
type rollingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	written int64
}

func newRollingWriter(path string, maxSize int64) (io.Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // Log directory needs to be readable
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Log file needs to be readable
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rollingWriter{
		path:    path,
		maxSize: maxSize,
		file:    f,
		written: info.Size(),
	}, nil
}

func (w *rollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.written+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			// Best-effort: keep writing to the current file if rotation fails.
			// Losing rotation is preferable to losing logs.
			_ = err
		}
	}

	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *rollingWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	rotated := w.path + ".1"
	_ = os.Remove(rotated)
	if err := os.Rename(w.path, rotated); err != nil {
		// Reopen the original file so we don't lose logging entirely.
		f, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Log file needs to be readable
		if openErr != nil {
			return openErr
		}
		w.file = f
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Log file needs to be readable
	if err != nil {
		return err
	}
	w.file = f
	w.written = 0
	return nil
}
