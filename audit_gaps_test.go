//go:build audit

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
)

// The recorder paths apply a committed save to the runtime (encoder.go AddRecorder/
// UpdateRecorder/RemoveRecorder); the key handler does not. When only the directory
// sync fails, the new key is live but the response withholds it, and /api/config only
// reports that a key exists, so the external recording API is locked out.
func TestAuditRegenerateKeyCommittedSaveReturnsLiveKey(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions as a regular user")
	}
	dir := t.TempDir()
	cfg := config.New(filepath.Join(dir, "config.json"))
	if err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	s := &Server{config: cfg, wsClients: map[chan any]struct{}{make(chan any, 1): {}}}
	// The rename into dir works; opening dir to sync it does not.
	if err := os.Chmod(dir, 0o300); err != nil { //nolint:gosec // An unreadable test directory is the point.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore for cleanup.

	rec := runJSONHandler(t, s.handleAPIRegenerateKey, http.MethodPost, "/api/regenerate-key", "")
	live := cfg.RecordingAPIKey()
	if live == "" {
		t.Fatal("no key was set")
	}
	if !strings.Contains(rec.Body.String(), live) {
		t.Fatalf("status %d, body %s: the live key %q is not returned", rec.Code, rec.Body.String(), live)
	}
}
