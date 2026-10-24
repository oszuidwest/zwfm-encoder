package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// ticketFor returns the ticket that owns path.
func ticketFor(ob *outbox, path string) (ticket, bool) {
	tickets := ob.snapshot()
	for i := range tickets {
		if tickets[i].Path == path {
			return tickets[i], true
		}
	}
	return ticket{}, false
}

// pendingTickets counts r's recordings that still wait for delivery.
func pendingTickets(r *GenericRecorder) int {
	n := 0
	tickets := r.outbox.snapshot()
	for i := range tickets {
		if tickets[i].State == ticketReady || tickets[i].State == ticketDelivered {
			n++
		}
	}
	return n
}

// assertRecordingQuarantined checks that file is failed, also after a restart.
func assertRecordingQuarantined(t *testing.T, r *GenericRecorder, file string) {
	t.Helper()
	if tk, ok := ticketFor(r.outbox, file); !ok || tk.State != ticketFailed {
		t.Fatalf("recording %s is not quarantined: %+v, %t", file, tk, ok)
	}
	reloaded, err := openOutbox(r.spoolDir, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	if tk, ok := ticketFor(reloaded, file); !ok || tk.State != ticketFailed {
		t.Fatalf("recording %s is not quarantined after a restart: %+v, %t", file, tk, ok)
	}
}

// deliverDue runs every upload that is due now, as the uploader would.
func deliverDue(t *testing.T, r *GenericRecorder) {
	t.Helper()
	u := &uploader{outbox: r.outbox, now: time.Now}
	for range 100 {
		tk, due, _ := r.outbox.nextDue(time.Now())
		if !due {
			return
		}
		u.process(t.Context(), &tk)
	}
	t.Fatal("uploads did not settle")
}

func TestOutboxLoadsNewestValidGeneration(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	ob, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	tk := ticket{Path: filepath.Join(spool, "a.mp3"), Dest: "d", RecorderID: "r1"}
	if err := os.WriteFile(tk.Path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ob.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	tk.State = ticketFailed
	if err := ob.update(&tk); err != nil {
		t.Fatal(err)
	}
	// A crash left an older generation behind and tore the next one.
	older := filepath.Join(ob.dir, tk.ID+".1.json")
	if err := os.WriteFile(older, []byte(`{"id":"`+tk.ID+`","seq":1,"state":"recording","path":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ob.dir, tk.ID+".3.json"), []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ticketFor(reloaded, tk.Path)
	if !ok || got.State != ticketFailed || got.Seq != 2 {
		t.Fatalf("reloaded ticket = %+v, %t; want generation 2 in state failed", got, ok)
	}
	entries, _ := os.ReadDir(ob.dir)
	if len(entries) != 2 { // The ticket and the destinations directory.
		t.Fatalf("stale generations left: %v", entries)
	}
	if err := reloaded.remove(&got); err != nil {
		t.Fatal(err)
	}
	if again, _ := openOutbox(spool, func(*ticket) {}); len(again.snapshot()) != 0 {
		t.Fatal("a removed ticket came back")
	}
}

// Generation files left ahead of a ticket must not swallow an update.
func TestOutboxUpdateSkipsTakenGenerations(t *testing.T) {
	t.Parallel()
	ob, err := openOutbox(t.TempDir(), func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	tk := ticket{Path: filepath.Join(t.TempDir(), "a.mp3")}
	if err := ob.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	for seq := tk.Seq + 1; seq <= tk.Seq+4; seq++ {
		if err := os.WriteFile(filepath.Join(ob.dir, fmt.Sprintf("%s.%d.json", tk.ID, seq)), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tk.State = ticketFailed
	if err := ob.update(&tk); err != nil {
		t.Fatal(err)
	}
	var stored ticket
	data, err := os.ReadFile(filepath.Join(ob.dir, fmt.Sprintf("%s.%d.json", tk.ID, tk.Seq)))
	if err != nil || json.Unmarshal(data, &stored) != nil || stored.State != ticketFailed {
		t.Fatalf("update persisted %q, %v; want state %s", data, err, ticketFailed)
	}
}

func TestOutboxRecoversInterruptedRecordings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dest   string
		synced int64
		file   *string     // nil: no file
		marker bool        // a .failed marker beside the file
		state  ticketState // the state before the restart; default recording
		want   ticketState
	}{
		{name: "audio, local only", file: ptr("partial hour"), want: ""},
		{name: "empty, never synced", dest: "d", file: ptr(""), want: ""},
		{name: "missing, never synced", dest: "d", want: ticketReady},
		{name: "empty after a sync", dest: "d", synced: 10, file: ptr(""), want: ticketLost},
		{name: "missing after a sync", dest: "d", synced: 10, want: ticketReady},
		{name: "shorter than synced", dest: "d", synced: 100, file: ptr("partial"), want: ticketReady},
		{name: "failure not persisted", dest: "d", file: ptr("partial"), marker: true, want: ticketFailed},
		{name: "failed, kept for the operator", dest: "d", file: ptr("partial"), state: ticketFailed, want: ticketFailed},
		{name: "failed, storage unavailable", dest: "d", state: ticketFailed, want: ticketFailed},
		{name: "lost, storage unavailable", dest: "d", state: ticketLost, want: ticketReady},
		{name: "lost, storage returned", dest: "d", file: ptr("partial"), state: ticketLost, want: ticketReady},
		{name: "missing, local only", want: ticketRecording},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spool := t.TempDir()
			ob, err := openOutbox(spool, func(*ticket) {})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(spool, "rec.mp3")
			if tt.file != nil {
				if err := os.WriteFile(path, []byte(*tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.marker {
				if err := os.WriteFile(path+failedMarkerSuffix, []byte("trailer\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tk := ticket{Path: path, Dest: tt.dest, SyncedBytes: tt.synced}
			if err := ob.newTicket(&tk); err != nil {
				t.Fatal(err)
			}
			if tt.state != "" {
				tk.State = tt.state
				if err := ob.update(&tk); err != nil {
					t.Fatal(err)
				}
			}

			interrupted := 0
			reloaded, err := openOutbox(spool, func(*ticket) { interrupted++ })
			if err != nil {
				t.Fatal(err)
			}
			assertTicketState(t, reloaded, path, tt.want)
			_, statErr := os.Stat(path)
			if keepsFile := tt.file != nil && (*tt.file != "" || tt.synced > 0); keepsFile != (statErr == nil) {
				t.Fatalf("file kept = %t, want %t", statErr == nil, keepsFile)
			}
			if wantLogged := tt.file != nil && *tt.file != "" && !tt.marker && (tt.state == "" || tt.state == ticketLost); (interrupted == 1) != wantLogged {
				t.Fatalf("interrupted events = %d, want logged %t", interrupted, wantLogged)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// I3: recovery hands an interrupted recording on only once its audio is
// durable; until then the next start tries again.
func TestRecoveryKeepsRecordingWhoseAudioCannotBeSynced(t *testing.T) {
	spool := t.TempDir()
	ob, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	tk := ticket{Path: filepath.Join(spool, "rec.mp3"), Dest: "d"}
	if err := os.WriteFile(tk.Path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ob.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	original := fsync
	t.Cleanup(func() { fsync = original })
	fsync = func(f *os.File) error {
		if f.Name() == tk.Path {
			return errors.New("sync failed")
		}
		return original(f)
	}
	reloaded, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	assertTicketState(t, reloaded, tk.Path, ticketRecording)
}

// I3: removing a ticket takes its newest generation last, after a barrier, so
// a failure or power cut never brings back an older state.
func TestTicketRemovalKeepsNewestGenerationUntilBarrier(t *testing.T) {
	ob, err := openOutbox(t.TempDir(), func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	tk := ticket{Path: "rec.mp3"}
	if err := ob.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	original := syncDir
	t.Cleanup(func() { syncDir = original })
	syncDir = func(string) error { return errors.New("sync failed") }
	if err := ob.removeFiles(&tk); err == nil {
		t.Fatal("a failed barrier was ignored")
	}
	if _, err := os.Stat(filepath.Join(ob.dir, tk.ID+"."+strconv.FormatUint(tk.Seq, 10)+ticketSuffix)); err != nil {
		t.Fatalf("newest generation removed before its barrier: %v", err)
	}
}

// assertTicketState checks path's ticket state; an empty want means no ticket.
func assertTicketState(t *testing.T, ob *outbox, path string, want ticketState) {
	t.Helper()
	got, ok := ticketFor(ob, path)
	if want == "" && ok || want != "" && (!ok || got.State != want) {
		t.Fatalf("ticket = %+v, %t; want state %q", got, ok, want)
	}
}

func TestOutboxKeepsTicketItCannotRead(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("file permissions do not stop root or Windows")
	}
	spool, cfg := t.TempDir(), localOnDemandRecorder(t, "Studio")
	cfg.RetentionDays = 1
	ob, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	tk := ticket{Path: filepath.Join(cfg.LocalPath, "Studio-2000-01-01-00-00.mp3"), Dest: "d"}
	if err := ob.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	writeCleanupFile(t, cfg.LocalPath, filepath.Base(tk.Path))
	if old := time.Now().AddDate(0, 0, -2); os.Chtimes(tk.Path, old, old) != nil {
		t.Fatal("cannot age the recording")
	}
	name := filepath.Join(ob.dir, tk.ID+".1.json")
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openOutbox(spool, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.unreadable != 1 {
		t.Fatalf("unreadable tickets = %d, want 1 reported", reloaded.unreadable)
	}
	if _, err := os.Lstat(name); err != nil {
		t.Fatalf("an unreadable ticket was deleted: %v", err)
	}
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, Outbox: reloaded})
	(&Manager{outbox: reloaded, recorders: map[string]*GenericRecorder{cfg.ID: r}}).runCleanup()
	if _, err := os.Lstat(tk.Path); err != nil {
		t.Fatalf("retention deleted a recording an unreadable ticket may own: %v", err)
	}
}
