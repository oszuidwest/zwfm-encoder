//go:build linux || darwin

package recording

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestStatfsSpaceExcludesReservedBlocks(t *testing.T) {
	t.Parallel()
	const bs = 4096
	for _, tt := range []struct {
		name                     string
		blocks, bfree, bavail    uint64
		wantAvailable, wantTotal uint64
	}{
		{"5% reserved", 1000, 300, 250, 250 * bs, 950 * bs},
		// Deleting every file must be able to reach the reported total.
		{"empty volume", 1000, 1000, 950, 950 * bs, 950 * bs},
		{"no reservation", 1000, 300, 300, 300 * bs, 1000 * bs},
		{"reserved space in use", 1000, 10, ^uint64(0) - 39, 0, 990 * bs},
	} {
		t.Run(tt.name, func(t *testing.T) {
			available, total, err := statfsSpace(tt.blocks, tt.bfree, tt.bavail, bs)
			if err != nil || available != tt.wantAvailable || total != tt.wantTotal {
				t.Fatalf("available=%d total=%d err=%v, want %d and %d", available, total, err, tt.wantAvailable, tt.wantTotal)
			}
		})
	}
}

func TestSpoolScanErrorKeepsLowSpoolHeld(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable directories")
	}
	m, _, logPath := localStorageManager(t)
	var callbacks int
	m.SetStorageChangeCallback(func([]types.RecordingStorageStatus) { callbacks++ })
	spool := filepath.Clean(m.spoolDir)
	var available uint64
	m.diskSpace = func(p string) (uint64, uint64, error) {
		if p == spool {
			return available, 20 << 30, nil
		}
		return 10 << 30, 20 << 30, nil
	}
	unreadable := filepath.Join(spool, "uploads", "recorder")
	if err := os.MkdirAll(unreadable, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o750) }) //nolint:gosec // G302: restore a test directory for TempDir cleanup.
	for _, step := range []struct {
		available uint64
		readable  bool
	}{
		{1 << 29, true},
		// Inside the recovery margin while the scan fails: the counts are valid, so it stays low.
		{3 << 29, false},
		{3 << 29, true},
		{1 << 29, true},
	} {
		mode := os.FileMode(0o750)
		if !step.readable {
			mode = 0
		}
		if err := os.Chmod(unreadable, mode); err != nil {
			t.Fatal(err)
		}
		available = step.available
		m.checkStorage(m.cleanupStopCh)
	}
	if low, restored := countStorageEvents(t, logPath); callbacks != 1 || low != 1 || restored != 0 {
		t.Fatalf("callbacks=%d low=%d restored=%d, want one alert and no recovery", callbacks, low, restored)
	}
}
