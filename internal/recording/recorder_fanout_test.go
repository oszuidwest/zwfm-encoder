package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestBlockedFilenameCheckDoesNotStopCaptureFanout(t *testing.T) {
	neverRotate(t)
	for _, tc := range []struct {
		name     string
		rotation bool
		update   bool
	}{
		{name: "startup"},
		{name: "rotation", rotation: true},
		{name: "rotation with concurrent config update", rotation: true, update: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(os.Args[0], t.TempDir(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			slowCfg := localOnDemandRecorder(t, "slow")
			slowCfg.RecordingMode, slowCfg.Enabled = types.RecordingHourly, true
			healthyCfg := localOnDemandRecorder(t, "healthy")
			for _, cfg := range []*types.Recorder{slowCfg, healthyCfg} {
				if err := m.AddRecorder(cfg); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = m.Stop() })
			slow, healthy := m.recorders[slowCfg.ID], m.recorders[healthyCfg.ID]
			files := []string{startCaptureTestRecorder(t, healthy)}
			if tc.rotation {
				files = append(files, startCaptureTestRecorder(t, slow))
			}
			entered, blocked := make(chan struct{}, 1), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			previousReserve := reserveRecordingFile
			reserveRecordingFile = func(path string) (*os.File, error) {
				if strings.HasPrefix(path, slowCfg.LocalPath+string(filepath.Separator)) {
					entered <- struct{}{}
					<-blocked
				}
				return previousReserve(path)
			}
			t.Cleanup(func() { reserveRecordingFile = previousReserve })
			defer release()
			rotated := make(chan struct{})
			if tc.rotation {
				go func() { rotateTestRecorder(slow); close(rotated) }()
			} else if err := slow.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("filename check did not block")
			}
			updated := make(chan error, 1)
			if tc.update {
				go func() { updated <- m.UpdateRecorder(slowCfg) }()
			} else {
				updated <- nil
			}
			pcm := []byte{1, 2, 3, 4}
			const chunks = 10
			delivered := make(chan struct{})
			go func() {
				for range chunks {
					m.WriteAudioShared(func() []byte { return pcm })
				}
				close(delivered)
			}()
			select {
			case <-delivered:
			case <-time.After(time.Second):
				t.Error("capture fan-out waited for the blocked filename check")
			}
			select {
			case err := <-updated:
				requireRecorderUpdate(t, err)
			case <-time.After(time.Second):
				t.Error("config update waited for the blocked filename check")
			}
			release()
			<-delivered
			if tc.rotation {
				<-rotated
			} else {
				waitForRecorderState(t, slow, types.ProcessRunning)
			}
			if err := m.Stop(); err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat(pcm, chunks)
			for _, file := range files {
				assertRecordedCapture(t, file, want)
			}
		})
	}
}

func requireRecorderUpdate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("recorder update failed: %v", err)
	}
}

func startCaptureTestRecorder(t *testing.T, r *GenericRecorder) string {
	t.Helper()
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitForRecorderState(t, r, types.ProcessRunning)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentFile
}

func assertRecordedCapture(t *testing.T, file string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(file) //nolint:gosec // Test temp recording.
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("recorded %s = %v, %v; want %v", filepath.Base(file), got, err, want)
	}
}
