//go:build windows

package util

import (
	"log/slog"
	"os"
	"path/filepath"
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
	localAppData, _ := os.UserCacheDir() // %LOCALAPPDATA%
	var paths []string
	for _, dir := range []string{os.Getenv("PROGRAMDATA"), localAppData, os.TempDir()} {
		if dir != "" {
			paths = append(paths, filepath.Join(dir, "encoder", "encoder.log"))
		}
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
		w, err := NewRollingWriter(path, slogMaxSizeBytes)
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
