package recording

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
			cfg := localOnDemandRecorder(t, "slow")
			cfg.RecordingMode = types.RecordingHourly
			m, r := newTestManager(t, cfg)
			if rotation {
				startCaptureTestRecorder(t, r)
			}
			entered, blocked, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			markEntered := sync.OnceFunc(func() { close(entered) })
			originalReserve := reserveRecordingFile
			reserveRecordingFile = func(path string) (*os.File, error) {
				file, err := originalReserve(path)
				if strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
					markEntered()
					<-blocked
				}
				return file, err
			}
			go func() {
				defer close(done)
				if rotation {
					rotateTestRecorder(r)
				} else if err := m.Start(); err != nil {
					t.Error(err)
				}
			}()
			t.Cleanup(func() { release(); <-done; reserveRecordingFile = originalReserve })
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
			waitForEmptyDir(t, cfg.LocalPath)
			if status := r.Status(); status.State != types.ProcessStopped || status.Error != "" || r.currentSegment() != nil {
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
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: executable, SpoolDir: t.TempDir()})
			entered, blocked := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			originalReserve := reserveRecordingFile
			blockFirst := sync.OnceFunc(func() { close(entered); <-blocked })
			reserveRecordingFile = func(path string) (*os.File, error) {
				file, err := originalReserve(path)
				if strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
					blockFirst()
				}
				return file, err
			}
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { release(); reserveRecordingFile = originalReserve })
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
				r.Stop()
				if err := r.Start(); err != nil {
					t.Fatal(err)
				}
				waitForRecorderState(t, r, types.ProcessRunning)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			newSegment := r.currentSegment()
			release()
			waitForEmptyDir(t, cfg.LocalPath)
			seg, state := r.currentSegment(), r.Status().State
			if state != types.ProcessRunning || !strings.HasPrefix(seg.file, filepath.Join(updated.LocalPath, updated.RecordingFilePrefix()+"-")) || seg.cfg.Name != updated.Name {
				t.Fatalf("prepared output used stale state: state=%s, file=%s", state, seg.file)
			}
			if seg != newSegment {
				t.Fatal("old preparation replaced the newer run's process")
			}
		})
	}
}

func TestHungFilenameCheckDoesNotMultiplyAcrossRestarts(t *testing.T) {
	cfg := localOnDemandRecorder(t, "hung")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
	var active, peak atomic.Int32
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	originalReserve := reserveRecordingFile
	reserveRecordingFile = func(path string) (*os.File, error) {
		if strings.HasPrefix(path, cfg.LocalPath+string(os.PathSeparator)) {
			now := active.Add(1)
			for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
			}
			<-blocked
			active.Add(-1)
		}
		return originalReserve(path)
	}
	// Five concurrent filename preparations share one blocked filesystem check.
	job := prepareJob{cfg: r.Config(), dir: cfg.LocalPath, lock: new(sync.Mutex), at: time.Now()}
	var starts sync.WaitGroup
	t.Cleanup(func() { release(); starts.Wait(); reserveRecordingFile = originalReserve })
	for range 5 {
		starts.Go(func() {
			if output, err := r.reserveOutput(&job); err == nil {
				output.discard()
			}
		})
	}
	// Directory preparation runs first; wait for the first check to block.
	for entered := time.Now().Add(5 * time.Second); time.Now().Before(entered) && peak.Load() < 1; {
		time.Sleep(5 * time.Millisecond)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && peak.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if peak.Load() != 1 {
		t.Fatalf("%d filename checks blocked at once, want exactly 1", peak.Load())
	}
}
