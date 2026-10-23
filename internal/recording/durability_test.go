package recording

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// I3: failed syncs neither advance the watermark nor hand the recording on.
func TestFailedSyncKeepsAudioUndurable(t *testing.T) {
	cfg := testS3Recorder()
	cfg.RecordingMode, cfg.StorageMode, cfg.LocalPath = types.RecordingOnDemand, types.StorageBoth, t.TempDir()
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0]})
	file := startCaptureTestRecorder(t, r)
	seg := r.currentSegment()
	seg.stopSync()
	seg.stopSync = func() {}
	if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil { // As FFmpeg would have.
		t.Fatal(err)
	}
	original := syncOutput
	t.Cleanup(func() { syncOutput = original })
	syncOutput = func(*os.File) error {
		if tk, ok := ticketFor(r.outbox, file); !ok || tk.State != ticketRecording {
			t.Errorf("ticket before sync = %+v, %t; want recording", tk, ok)
		}
		return errors.New("sync failed")
	}
	output := r.currentSegment().output
	if err := output.sync(); err == nil {
		t.Fatal("a failed sync was reported as done")
	}
	if tk, _ := ticketFor(r.outbox, file); tk.SyncedBytes != 0 {
		t.Fatalf("watermark = %d after a failed sync, want 0", tk.SyncedBytes)
	}
	failedSync := syncOutput
	syncOutput = original
	if err := output.sync(); err != nil {
		t.Fatal(err)
	}
	if tk, _ := ticketFor(r.outbox, file); tk.SyncedBytes != int64(len("audio")) {
		t.Fatalf("watermark = %d after a sync, want %d", tk.SyncedBytes, len("audio"))
	}
	syncOutput = failedSync
	r.WriteAudio([]byte{1, 2, 3, 4})
	r.Stop()
	assertRecordingQuarantined(t, r, file)
}

// I3: a directory whose new entry could not be synced is synced again when
// it is used next, although it exists by then.
func TestDirectoryBarrierIsRetried(t *testing.T) {
	for name, prepare := range map[string]func(string) error{
		"spool parent": func(dir string) error {
			m, err := NewManager("", dir, 0, nil)
			if err == nil {
				m.Close()
			}
			return err
		},
		"recording parent":      func(dir string) error { return prepareRecordingDir(types.StorageS3, dir) },
		"recording grandparent": func(dir string) error { return prepareRecordingDir(types.StorageS3, dir) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "recordings", "new")
			barrier := filepath.Dir(dir)
			if name == "recording grandparent" {
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
}
