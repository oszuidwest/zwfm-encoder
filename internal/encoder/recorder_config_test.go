package encoder

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestRecorderRuntimeFailureReportsPersistedChange(t *testing.T) {
	// A fresh add cannot fail at runtime: the config generates a new ID.
	for _, operation := range []string{"update", "remove"} {
		t.Run(operation, func(t *testing.T) {
			e := newSourceLifecycleTestEncoder(t, "exit")
			t.Cleanup(e.alertOrchestrator.Close)
			path, spool := filepath.Join(t.TempDir(), "config.json"), t.TempDir()
			e.config = config.New(path)
			if err := e.config.Load(); err != nil {
				t.Fatal(err)
			}
			var err error
			e.recordingManager, err = recording.NewManager("", spool, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			recorder := &types.Recorder{Name: "Before", Codec: types.CodecMP3, RecordingMode: types.RecordingOnDemand,
				StorageMode: types.StorageLocal, LocalPath: t.TempDir()}
			// Config exists, but runtime has lost the recorder.
			if err := e.config.AddRecorder(recorder); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "update":
				recorder.Name = "After"
				err = e.UpdateRecorder(recorder)
			case "remove":
				err = e.RemoveRecorder(recorder.ID)
			}
			switch {
			case operation == "remove" && err != nil:
				// A recorder the runtime never had is already in the wanted state.
				t.Fatalf("removing a recorder missing from the runtime = %v, want success", err)
			case operation != "remove" && !errors.Is(err, config.ErrSaveCommitted):
				t.Fatalf("runtime failure = %v, want it reported as a saved change", err)
			}
			reloaded := config.New(path)
			if err := reloaded.Load(); err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []*config.Config{e.config, reloaded} {
				got := cfg.Recorder(recorder.ID)
				if operation == "remove" {
					if got != nil {
						t.Fatal("removal was not persisted")
					}
				} else if got == nil || got.Name != recorder.Name {
					t.Fatalf("reported saved state differs from config: %+v", got)
				}
			}
		})
	}
}

func TestRecorderSaveFailureDoesNotRemoveRuntimeRecorder(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	path := filepath.Join(t.TempDir(), "config.json")
	e.config = config.New(path)
	if err := e.config.Load(); err != nil {
		t.Fatal(err)
	}
	recorder := &types.Recorder{Name: "Keep", Codec: types.CodecMP3, RecordingMode: types.RecordingOnDemand,
		StorageMode: types.StorageLocal, LocalPath: t.TempDir()}
	if err := e.AddRecorder(recorder); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block-rename"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveRecorder(recorder.ID); err == nil {
		t.Fatal("removal succeeded despite failed save")
	}
	if _, exists := e.RecorderStatuses()[recorder.ID]; !exists || e.config.Recorder(recorder.ID) == nil {
		t.Fatal("failed save removed the recorder from runtime or config")
	}
}

func TestCommittedRecorderChangeReachesRuntimeWhenSyncFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions as a regular user")
	}
	for _, operation := range []string{"add", "update"} {
		t.Run(operation, func(t *testing.T) {
			e := newSourceLifecycleTestEncoder(t, "exit")
			t.Cleanup(e.alertOrchestrator.Close)
			dir := t.TempDir()
			e.config = config.New(filepath.Join(dir, "config.json"))
			if err := e.config.Load(); err != nil {
				t.Fatal(err)
			}
			var err error
			e.recordingManager, err = recording.NewManager("", t.TempDir(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			recorder := &types.Recorder{Name: "Before", Codec: types.CodecMP3, RecordingMode: types.RecordingOnDemand,
				StorageMode: types.StorageLocal, LocalPath: t.TempDir()}
			if operation == "update" {
				if err := e.AddRecorder(recorder); err != nil {
					t.Fatal(err)
				}
				recorder.Name = "After"
				// Without it in the runtime, the attempt shows up in the error.
				if err := e.recordingManager.RemoveRecorder(recorder.ID); err != nil {
					t.Fatal(err)
				}
			}
			// The rename into dir still works; opening dir to sync it does not.
			if err := os.Chmod(dir, 0o300); err != nil { //nolint:gosec // An unreadable test directory is the point.
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore for cleanup.
			if operation == "add" {
				err = e.AddRecorder(recorder)
			} else {
				err = e.UpdateRecorder(recorder)
			}
			if !errors.Is(err, config.ErrSaveCommitted) {
				t.Fatalf("sync failure = %v, want a committed change", err)
			}
			if _, running := e.recordingManager.Statuses()[recorder.ID]; operation == "add" && !running {
				t.Fatal("committed recorder was not added to the runtime")
			}
			if operation == "update" && !strings.Contains(err.Error(), "update recorder") {
				t.Fatalf("committed update never reached the runtime: %v", err)
			}
		})
	}
}
