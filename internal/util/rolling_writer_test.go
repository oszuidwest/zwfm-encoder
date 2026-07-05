package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMaxSize int64 = 64

func newTestWriter(t *testing.T) (w *rollingWriter, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "encoder.log")
	w, err := newRollingWriter(path, testMaxSize)
	if err != nil {
		t.Fatalf("newRollingWriter() error = %v", err)
	}
	return w, path
}

func mustWrite(t *testing.T, w *rollingWriter, s string) {
	t.Helper()
	n, err := w.Write([]byte(s))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(s) {
		t.Fatalf("Write() = %d, want %d", n, len(s))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // Test-controlled path
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(b)
}

func TestRollingWriterRotatesAtCap(t *testing.T) {
	t.Parallel()
	w, path := newTestWriter(t)

	first := strings.Repeat("a", 60)
	second := strings.Repeat("b", 10)
	mustWrite(t, w, first)
	mustWrite(t, w, second) // 60+10 > 64: rotates before writing

	if got := readFile(t, path+".1"); got != first {
		t.Fatalf("rotated file = %q, want %q", got, first)
	}
	if got := readFile(t, path); got != second {
		t.Fatalf("active file = %q, want %q", got, second)
	}
}

func TestRollingWriterRolloverReplacesPrevious(t *testing.T) {
	t.Parallel()
	w, path := newTestWriter(t)

	gen1 := strings.Repeat("1", 60)
	gen2 := strings.Repeat("2", 60)
	last := strings.Repeat("x", 5)
	mustWrite(t, w, gen1)
	mustWrite(t, w, gen2) // rotates gen1 to .1
	mustWrite(t, w, last) // 60+5 > 64: rotates gen2, replacing .1

	if got := readFile(t, path+".1"); got != gen2 {
		t.Fatalf("rotated file = %q, want %q", got, gen2)
	}
	if got := readFile(t, path); got != last {
		t.Fatalf("active file = %q, want %q", got, last)
	}
}

func TestRollingWriterResumesFromExistingSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "encoder.log")
	existing := strings.Repeat("e", 40)
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil { //nolint:gosec // Test log file
		t.Fatalf("WriteFile() error = %v", err)
	}

	w, err := newRollingWriter(path, testMaxSize)
	if err != nil {
		t.Fatalf("newRollingWriter() error = %v", err)
	}
	if w.written != 40 {
		t.Fatalf("written = %d, want 40 (resumed from existing size)", w.written)
	}

	next := strings.Repeat("n", 30)
	mustWrite(t, w, next) // 40+30 > 64: rotates the pre-existing content

	if got := readFile(t, path+".1"); got != existing {
		t.Fatalf("rotated file = %q, want %q", got, existing)
	}
	if got := readFile(t, path); got != next {
		t.Fatalf("active file = %q, want %q", got, next)
	}
}

func TestRollingWriterOversizedWrite(t *testing.T) {
	t.Parallel()
	w, path := newTestWriter(t)

	big := strings.Repeat("B", 100) // single record above the cap
	mustWrite(t, w, big)
	if got := readFile(t, path); got != big {
		t.Fatalf("active file = %q, want %q", got, big)
	}

	mustWrite(t, w, "x") // 100+1 > 64: oversized file rotates out intact
	if got := readFile(t, path+".1"); got != big {
		t.Fatalf("rotated file = %q, want %q", got, big)
	}
	if got := readFile(t, path); got != "x" {
		t.Fatalf("active file = %q, want %q", got, "x")
	}
}

func TestRollingWriterSurvivesRenameFailure(t *testing.T) {
	t.Parallel()
	w, path := newTestWriter(t)

	// A non-empty directory at the rollover path makes both the Remove and
	// the Rename inside rotateLocked fail, on every platform.
	rotated := path + ".1"
	if err := os.Mkdir(rotated, 0o750); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rotated, "block"), []byte("x"), 0o644); err != nil { //nolint:gosec // Test fixture
		t.Fatalf("WriteFile() error = %v", err)
	}

	first := strings.Repeat("a", 60)
	second := strings.Repeat("b", 10)
	mustWrite(t, w, first)
	mustWrite(t, w, second) // rotation fails; the write must still land

	if got := readFile(t, path); got != first+second {
		t.Fatalf("active file = %q, want old content plus new record", got)
	}

	// Counter was reset on the failed rotation, so the retry comes after
	// another cap's worth of bytes - and succeeds once the blocker is gone.
	if err := os.RemoveAll(rotated); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	third := strings.Repeat("c", 60)
	mustWrite(t, w, third) // 10+60 > 64: rotation retried, now succeeds

	if got := readFile(t, path+".1"); got != first+second {
		t.Fatalf("rotated file = %q, want the previously stuck content", got)
	}
	if got := readFile(t, path); got != third {
		t.Fatalf("active file = %q, want %q", got, third)
	}
}

func TestRollingWriterSelfHealsAfterReopenFailure(t *testing.T) {
	t.Parallel()
	w, path := newTestWriter(t)
	mustWrite(t, w, "before")

	// Simulate a failed rotate/reopen: the writer is marked broken and the
	// path is temporarily unopenable (a directory in its place).
	_ = w.file.Close()
	w.file = nil
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if _, err := w.Write([]byte("lost")); err == nil {
		t.Fatal("Write() error = nil, want open failure while path is a directory")
	}

	// Blocker gone: the next Write must reopen and succeed on its own.
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	mustWrite(t, w, "after")

	if got := readFile(t, path); got != "after" {
		t.Fatalf("active file = %q, want %q", got, "after")
	}
}
