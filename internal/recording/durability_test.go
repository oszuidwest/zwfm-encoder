package recording

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// failSyncOutput makes recording syncs fail until the returned function is called.
func failSyncOutput(t *testing.T) (heal func()) {
	t.Helper()
	original := syncOutput
	syncOutput = func(*os.File) error { return errors.New("sync failed") }
	heal = sync.OnceFunc(func() { syncOutput = original })
	t.Cleanup(heal)
	return heal
}

// I3: audio counts as durable, and is handed on, only after a sync succeeded.
func TestFailedSyncKeepsAudioUndurable(t *testing.T) {
	t.Run("periodic", func(t *testing.T) {
		r := newTestRecorder(t, GenericRecorderConfig{Recorder: localOnDemandRecorder(t, "periodic"), FFmpegPath: os.Args[0]})
		file := startCaptureTestRecorder(t, r)
		if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil { // As FFmpeg would have.
			t.Fatal(err)
		}
		output := r.currentSegment().output
		heal := failSyncOutput(t)
		if err := output.sync(); err == nil {
			t.Fatal("a failed sync was reported as done")
		}
		if tk, _ := ticketFor(r.outbox, file); tk.SyncedBytes != 0 {
			t.Fatalf("watermark = %d after a failed sync, want 0", tk.SyncedBytes)
		}
		heal()
		if err := output.sync(); err != nil {
			t.Fatal(err)
		}
		if tk, _ := ticketFor(r.outbox, file); tk.SyncedBytes != int64(len("audio")) {
			t.Fatalf("watermark = %d after a sync, want %d", tk.SyncedBytes, len("audio"))
		}
	})
	t.Run("final", func(t *testing.T) {
		cfg := testS3Recorder()
		cfg.RecordingMode, cfg.StorageMode, cfg.LocalPath = types.RecordingOnDemand, types.StorageBoth, t.TempDir()
		r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
		file := startCaptureTestRecorder(t, r)
		r.WriteAudio([]byte{1, 2, 3, 4})
		failSyncOutput(t)
		failedSync := syncOutput
		syncOutput = func(f *os.File) error {
			if tk, ok := ticketFor(r.outbox, file); !ok || tk.State != ticketRecording {
				t.Errorf("ticket before final sync = %+v, %t; want recording", tk, ok)
			}
			return failedSync(f)
		}
		r.Stop()
		assertRecordingQuarantined(t, r, file)
		if st := r.Status(); !strings.Contains(st.Error, "sync recording to disk") {
			t.Fatalf("status = %+v, want the sync failure", st)
		}
	})
}

// I3: a directory whose new entry could not be synced is synced again when
// it is used next, although it exists by then.
func TestDirectoryBarrierIsRetried(t *testing.T) {
	for name, prepare := range map[string]func(dir string) error{
		"spool": func(dir string) error {
			m, err := NewManager("", dir, 0, nil)
			if err == nil {
				m.Close()
			}
			return err
		},
		"recording directory": func(dir string) error { return prepareRecordingDir(types.StorageS3, dir) },
	} {
		t.Run(name, func(t *testing.T) {
			for _, level := range []string{"parent", "grandparent"} {
				if name == "spool" && level == "grandparent" {
					continue
				}
				t.Run(level, func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "recordings", "new")
					barrier := filepath.Dir(dir)
					if level == "grandparent" {
						barrier = filepath.Dir(barrier)
					}
					var synced []string
					failing := true
					original := syncDir
					syncDir = func(d string) error {
						synced = append(synced, d)
						if failing && d == barrier {
							return errors.New("sync failed")
						}
						return original(d)
					}
					t.Cleanup(func() { syncDir = original })
					if err := prepare(dir); err == nil {
						t.Fatal("a failed directory sync was ignored")
					}
					if err := prepare(dir); err == nil {
						t.Error("a failed barrier on an existing directory was ignored")
					}
					failing, synced = false, nil
					if err := prepare(dir); err != nil {
						t.Fatal(err)
					}
					if !slices.Contains(synced, barrier) {
						t.Fatalf("retry synced %v, want %s again", synced, barrier)
					}
				})
			}
		})
	}
}
