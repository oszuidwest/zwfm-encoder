package recording

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestBlockedFilenameCheckDoesNotBlockRecorderControls(t *testing.T) {
	neverRotate(t)
	for _, rotation := range []bool{false, true} {
		name := "startup"
		if rotation {
			name = "rotation"
		}
		t.Run(name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(executable, t.TempDir(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg := localOnDemandRecorder(t, "slow")
			cfg.RecordingMode = types.RecordingHourly
			if err := m.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			r := m.recorders[cfg.ID]
			if rotation {
				startCaptureTestRecorder(t, r)
			} else {
				r.state, r.startGen = types.ProcessStarting, 1
			}
			entered, blocked, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			markEntered := sync.OnceFunc(func() { close(entered) })
			originalReserve := reserveRecordingFile
			reserveRecordingFile = func(path string) (*os.File, error) {
				if strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
					markEntered()
					<-blocked
				}
				return originalReserve(path)
			}
			go func() {
				defer close(done)
				if rotation {
					rotateTestRecorder(r)
				} else {
					r.startAsync(1)
				}
			}()
			t.Cleanup(func() { release(); <-done; reserveRecordingFile = originalReserve; _ = m.Stop() })
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("filename check did not block")
			}
			for _, operation := range []struct {
				name string
				run  func() error
			}{
				{"status", func() error { _ = m.Statuses(); return nil }},
				{"update", func() error { return m.UpdateRecorder(cfg) }},
				{"stop", m.Stop},
			} {
				result := make(chan error, 1)
				go func() { result <- operation.run() }()
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Errorf("%s waited for the blocked filename check", operation.name)
					release()
					<-result
				}
			}
			release()
			<-done
			if status := r.Status(); status.State != types.ProcessStopped || status.Error != "" || r.result != nil {
				t.Fatalf("obsolete file preparation changed the stopped recorder: %+v", status)
			}
		})
	}
}

func TestFilenamePreparationRevalidatesConfigAndRun(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "config changed"
		if restart {
			name = "new run"
		}
		t.Run(name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cfg := localOnDemandRecorder(t, "test")
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: executable, SpoolDir: t.TempDir()})
			r.state, r.startGen = types.ProcessStarting, 1
			entered, blocked, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			originalReserve := reserveRecordingFile
			blockFirst := sync.OnceFunc(func() { close(entered); <-blocked })
			reserveRecordingFile = func(path string) (*os.File, error) {
				if strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
					blockFirst()
				}
				return originalReserve(path)
			}
			go func() { defer close(done); r.startAsync(1) }()
			t.Cleanup(func() { release(); <-done; reserveRecordingFile = originalReserve; _ = r.Stop() })
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("filename check did not block")
			}
			updated := *cfg
			updated.Name, updated.LocalPath = "Updated", t.TempDir()
			if err := r.UpdateConfig(&updated); err != nil {
				t.Fatal(err)
			}
			if restart {
				if err := r.Stop(); err != nil {
					t.Fatal(err)
				}
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				waitForRecorderState(t, r, types.ProcessRunning)
			}
			r.mu.RLock()
			newResult := r.result
			r.mu.RUnlock()
			release()
			<-done
			if files, err := os.ReadDir(cfg.LocalPath); err != nil || len(files) != 0 {
				t.Fatalf("stale configuration left recording reservations: %v, %v", files, err)
			}
			r.mu.RLock()
			defer r.mu.RUnlock()
			if r.state != types.ProcessRunning || !strings.HasPrefix(r.currentFile, filepath.Join(updated.LocalPath, updated.RecordingFilePrefix()+"-")) || r.config.Name != updated.Name {
				t.Fatalf("prepared output used stale state: state=%s, file=%s", r.state, r.currentFile)
			}
			if restart && r.result != newResult {
				t.Fatal("old preparation replaced the newer run's process")
			}
		})
	}
}
