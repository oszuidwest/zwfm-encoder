package decodermonitor

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testStore(t *testing.T) (Config, *Store) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local watchdog storage runs on Linux")
	}
	cfg := DefaultConfig()
	cfg.Site = "Test"
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	s, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cfg, s
}

func TestStoreAtomicStateEvidenceAndRestart(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if second, err := OpenStore(&cfg); err == nil {
		_ = second.Close()
		t.Fatal("two watchdogs own the same state")
	}
	at := time.Now()
	records := []Record{impactRecord(at)}
	if err := store.Update(records, at, func(s *State) error {
		s.Bookmark = Bookmark{Cursor: "saved", Key: "proof"}
		s.Incident = Incident{ID: 1, Severity: "critical"}
		s.Outbox = []Mail{{ID: 2, Subject: "pending", Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(records, at, func(_ *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "evidence"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("evidence replay duplicated or lost: %d %v", len(entries), err)
	}
	for _, path := range []string{filepath.Join(cfg.StateDir, "state.json"), filepath.Join(cfg.StateDir, "evidence", entries[0].Name())} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("non-private evidence/state: %s %v", path, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	s, err := reopened.Snapshot()
	if err != nil || s.Bookmark.Cursor != "saved" || s.Incident.Severity != "critical" || len(s.Outbox) != 1 {
		t.Fatalf("state lost on restart: %+v %v", s, err)
	}
}

func TestStoreFailedTransitionDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()
	_, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Bookmark.Cursor = "old"; return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("do not commit")
	err := store.Update([]Record{impactRecord(at)}, at, func(s *State) error { s.Bookmark.Cursor = "new"; return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil || state.Bookmark.Cursor != "old" {
		t.Fatalf("failed transition committed: %+v %v", state, err)
	}
	store.maxEvidence = 1
	if err := store.Update([]Record{impactRecord(at)}, at, func(s *State) error { s.Bookmark.Cursor = "new"; return nil }); err == nil {
		t.Fatal("over-quota batch committed")
	}
	state, err = store.Snapshot()
	if err != nil || state.Bookmark.Cursor != "old" {
		t.Fatal("evidence failure advanced cursor")
	}
}

func TestStoreRestartRemovesOnlyOrphanTransactions(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cfg.StateDir, filepath.Join(cfg.StateDir, "evidence")} {
		for _, name := range []string{".watchdog-12345.tmp", "keep.tmp"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("interrupted write"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, ".watchdog-directory.tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("keep.tmp", filepath.Join(dir, ".watchdog-link.tmp")); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for _, dir := range []string{cfg.StateDir, filepath.Join(cfg.StateDir, "evidence")} {
		if _, err := os.Stat(filepath.Join(dir, ".watchdog-12345.tmp")); !os.IsNotExist(err) {
			t.Fatal("orphan transaction retained")
		}
		for _, name := range []string{"keep.tmp", ".watchdog-directory.tmp", ".watchdog-link.tmp"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("unowned/non-regular path removed: %s: %v", name, err)
			}
		}
	}
}

func TestStoreCorruptionAndSymlinkFailClosed(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(&cfg); err == nil {
		_ = s.Close()
		t.Fatal("corrupt state silently discarded")
	}
	if err := os.Remove(filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(&cfg); err == nil {
		_ = s.Close()
		t.Fatal("state symlink escaped root")
	}
}

func TestEvidenceRetentionDoesNotTouchUnownedFiles(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update([]Record{impactRecord(at)}, at, func(_ *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(cfg.StateDir, "evidence", entries[0].Name())
	if err := os.Chtimes(old, at.Add(-8*24*time.Hour), at.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(cfg.StateDir, "evidence", "keep.txt")
	if err := os.WriteFile(other, []byte("not watchdog evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired batch retained")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unowned file removed")
	}
}
