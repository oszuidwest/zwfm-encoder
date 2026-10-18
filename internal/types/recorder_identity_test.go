package types

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestRecorderIdentityIsLosslessAndCaseIndependent(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, id := range []string{"abc", "ABC", "a/b", "a\\b", "a-b", "..", "日本"} {
		r := Recorder{ID: id, Name: "Studio B!"}
		r.InitializeFileIdentity()
		prefix := r.RecordingFilePrefix()
		decoded, err := hex.DecodeString(strings.TrimPrefix(prefix, "@"))
		if err != nil || string(decoded) != id || seen[strings.ToLower(prefix)] {
			t.Fatalf("unsafe or lossy prefix for %q: %q, %v", id, prefix, err)
		}
		seen[strings.ToLower(prefix)] = true
		r.Name = "Renamed"
		r.InitializeFileIdentity()
		if r.LegacyFilePrefix != "Studio-B" || r.RecordingFilePrefix() != prefix {
			t.Fatal("reinitialization changed the immutable ownership")
		}
	}
	for _, id := range []string{strings.Repeat("a", 250), strings.Repeat("b", 250)} {
		r := Recorder{ID: id}
		prefix := r.RecordingFilePrefix()
		if !strings.HasPrefix(prefix, "@h") || len(prefix) != 66 || seen[prefix] {
			t.Fatalf("long ID has an unbounded or overlapping namespace: %q", prefix)
		}
		seen[prefix] = true
	}
}

func TestGeneratedRecorderIDsKeepReadablePrefixes(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"recorder-1a2b3c": "@recorder-1a2b3c",
		// Case variants and other forms are hex-encoded, so they stay apart.
		"recorder-1A2B3C": "@" + hex.EncodeToString([]byte("recorder-1A2B3C")),
		"recorder-":       "@" + hex.EncodeToString([]byte("recorder-")),
		"recorder-xyz":    "@" + hex.EncodeToString([]byte("recorder-xyz")),
	} {
		if got := (&Recorder{ID: id}).RecordingFilePrefix(); got != want {
			t.Errorf("prefix of %q = %q, want %q", id, got, want)
		}
	}
}

func TestInitializeFileIdentityKeepsRestoredLegacyPrefix(t *testing.T) {
	t.Parallel()
	r := Recorder{ID: "r1", Name: "Renamed", LegacyFilePrefix: "Before"} // Restored after a downgrade.
	r.InitializeFileIdentity()
	if !r.IDBasedFiles || r.LegacyFilePrefix != "Before" {
		t.Fatalf("restored legacy prefix overwritten: %+v", r)
	}
}
