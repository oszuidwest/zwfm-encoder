package decodermonitor

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalLine(t *testing.T, cursor, message, timestamp string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"__CURSOR": cursor, "_BOOT_ID": "a-boot", "__REALTIME_TIMESTAMP": timestamp, "__MONOTONIC_TIMESTAMP": "1234", "_PID": "100", "MESSAGE": message})
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestJournalOpaqueCursorAliasAndLostAnchor(t *testing.T) {
	t.Parallel()
	first := journalLine(t, "opaque-original", "Buffer filling", "1792397600000000")
	record, err := decodeRecord([]byte(first))
	if err != nil {
		t.Fatal(err)
	}
	bookmark := Bookmark{Cursor: record.Cursor, Key: record.Key}
	alias := journalLine(t, "different-opaque-alias", "Buffer filling", "1792397600000000")
	next := journalLine(t, "next", "Buffer empty", "1792397610000000")
	result, err := parseJournal(strings.NewReader(alias+next), bookmark)
	if err != nil || result.Continuity != "ok" || !result.CaughtUp || len(result.Records) != 1 || result.Cursor != "next" {
		t.Fatalf("opaque alias rejected: %+v %v", result, err)
	}
	for _, input := range []string{next, ""} {
		result, err := parseJournal(strings.NewReader(input), bookmark)
		if err != nil || result.Continuity != "gap" || result.CaughtUp {
			t.Fatalf("missing anchor was trusted: %+v %v", result, err)
		}
	}
}

func TestJournalBoundsAndMalformedRecords(t *testing.T) {
	t.Parallel()
	var input strings.Builder
	for i := range maxRecords + 1 {
		input.WriteString(journalLine(t, string(rune('a'+i)), "Buffer filling", "1792397600000000"))
	}
	result, err := parseJournal(strings.NewReader(input.String()), Bookmark{})
	if err != nil || result.CaughtUp || len(result.Records) != maxRecords {
		t.Fatalf("bad record bound: %d %v %v", len(result.Records), result.CaughtUp, err)
	}
	for _, text := range []string{"not json", `{}`, `{"MESSAGE":[10,20]}`, strings.Repeat("a", 130<<10)} {
		if _, err := parseJournal(strings.NewReader(text), Bookmark{}); err == nil {
			t.Fatal("malformed/oversize journal accepted")
		}
	}
	buffer := &boundedBuffer{limit: 2}
	data := []byte{'a', 'b', 'c'}
	if _, err := buffer.Write(data); err == nil {
		t.Fatal("unbounded output")
	}
	if buffer.Len() != 0 {
		t.Fatal("partial over-limit data retained")
	}
	if _, err := io.Copy(buffer, strings.NewReader("abc")); err == nil || buffer.Len() != 0 {
		t.Fatal("io.Copy bypassed the subprocess output bound")
	}
}

func TestReadPCMUsesProcStatusWithoutOpeningAudio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, path := range []string{"asound/hifiberry/pcm0p/sub0", "321"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	status := "state: RUNNING\nowner_pid: 321\ntrigger_time: 123.456\nhw_ptr: 1000\nappl_ptr: 2000\n"
	path := filepath.Join(dir, "asound", "hifiberry", "pcm0p", "sub0", "status")
	if err := os.WriteFile(path, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "321", "status"), []byte("Name: MicroMPX\nTgid: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	pcm := readPCM(root, "/proc/asound/hifiberry/pcm0p/sub0/status")
	if pcm.State != "RUNNING" || pcm.Owner != 100 || pcm.Hardware != 1000 || pcm.Applied != 2000 {
		t.Fatalf("bad PCM parse: %+v", pcm)
	}
	if err := os.WriteFile(path, []byte("closed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPCM(root, "/proc/asound/hifiberry/pcm0p/sub0/status"); got.State != "closed" {
		t.Fatal(got)
	}
	if got := readPCM(root, "/proc/asound/../../etc/passwd"); got.State != "unavailable" {
		t.Fatal("unsafe proc path accepted")
	}
}
