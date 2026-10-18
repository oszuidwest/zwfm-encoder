package recording

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// storageTestManager enables explicit synchronous checks without schedulers.
func storageTestManager(t *testing.T, spool string) *Manager {
	t.Helper()
	m, err := NewManager("", spool, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.running, m.cleanupStopCh = true, make(chan struct{})
	m.diskSpace = func(string) (uint64, uint64, error) { return 10 << 30, 20 << 30, nil }
	return m
}

func TestStorageWarningsThresholdsDedupAndRecovery(t *testing.T) {
	t.Parallel()
	m := storageTestManager(t, t.TempDir())
	cfg := localOnDemandRecorder(t, "local")
	cfg.Enabled = true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	other := *cfg
	other.ID, other.Name = "shared", "shared"
	if err := m.AddRecorder(&other); err != nil {
		t.Fatal(err)
	}
	disabled := *cfg
	disabled.ID, disabled.Name, disabled.LocalPath, disabled.Enabled = "disabled", "disabled", "/unused", false
	if err := m.AddRecorder(&disabled); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	m.eventLogger = logger
	var callbacks int
	m.SetStorageChangeCallback(func(paths []types.RecordingStorageStatus) {
		callbacks++
		// Both accessors must be callable while an alert is being emitted.
		_ = m.StorageStatuses()
		_ = m.Statuses()
		paths[0].Path = "mutated by callback"
	})
	tests := []struct {
		name             string
		available, total uint64
		err              error
		low              bool
		callbacks        int
	}{
		{"healthy", 2 << 30, 20 << 30, nil, false, 0},
		{"absolute threshold", (1 << 30) - 1, 10 << 30, nil, true, 1},
		{"still low", 0, 10 << 30, nil, true, 1},
		{"threshold inclusive", 1 << 30, 10 << 30, nil, false, 2},
		{"relative threshold", 2 << 30, 100 << 30, nil, true, 3},
		{"probe error is not recovery", 0, 0, errors.New("disk offline"), false, 3},
		{"healthy again", 10 << 30, 100 << 30, nil, false, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			m.diskSpace = func(string) (uint64, uint64, error) { calls++; return tt.available, tt.total, tt.err }
			m.checkStorage(m.cleanupStopCh)
			paths := m.StorageStatuses()
			if calls != 2 || len(paths) != 2 || callbacks != tt.callbacks {
				t.Fatalf("probe calls=%d paths=%+v callbacks=%d", calls, paths, callbacks)
			}
			for _, p := range paths {
				if p.Low != tt.low || (p.Error != "") != (tt.err != nil) || p.Path == "mutated by callback" || p.CheckedAt.IsZero() {
					t.Fatalf("unexpected observation: %+v", p)
				}
			}
			paths[0].Path = "mutated by reader"
			if m.StorageStatuses()[0].Path == "mutated by reader" {
				t.Fatal("status accessor returned mutable manager state")
			}
		})
	}
	events, _, err := eventlog.ReadLast(logPath, 100, 0, eventlog.FilterRecorder)
	if err != nil || len(events) != 8 {
		t.Fatalf("transition events=%+v err=%v; want two paths, four transitions", events, err)
	}
}

func TestBlockedStorageProbeDoesNotBlockCaptureStatusOrRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, testBlockedStorageProbe)
}

func testBlockedStorageProbe(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testS3Recorder()
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	recorder := m.recorders[cfg.ID]
	recorder.audioCh = make(chan []byte, 1)
	captured := recorder.audioCh
	recorder.recording.Store(true)
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseProbe := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseProbe)
	var probes atomic.Int32
	m.diskSpace = func(string) (uint64, uint64, error) {
		if probes.Add(1) == 1 {
			close(entered)
			<-release
			defer close(exited)
		}
		return 0, 10 << 30, nil
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	oldStop := m.cleanupStopCh
	done := make(chan error, 1)
	go func() {
		m.WriteAudio([]byte{0, 0})
		_ = m.Statuses()
		_ = m.StorageStatuses()
		_ = m.AbandonedUploadCount()
		if err := m.Stop(); err != nil {
			done <- err
			return
		}
		done <- m.Start()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		releaseProbe()
		t.Fatal("blocked probe blocked capture/status/stop/restart")
	}
	select {
	case pcm := <-captured:
		if len(pcm) != 2 {
			t.Fatalf("unexpected captured PCM: %v", pcm)
		}
	case <-time.After(time.Second):
		t.Fatal("capture was not delivered while the probe was blocked")
	}
	// Explicit new-run checks do not spawn or join another blocked OS probe.
	m.checkStorage(m.cleanupStopCh)
	if probes.Load() != 1 {
		t.Fatalf("concurrent probes=%d", probes.Load())
	}
	synctest.Wait() // The new monitor has skipped the busy probe and is awaiting its ticker.
	releaseProbe()
	<-exited
	// Wait for the old probe to pass its publication guard and release ownership.
	func() {
		m.storageProbeMu.Lock()
		defer m.storageProbeMu.Unlock()
	}()
	if len(m.StorageStatuses()) != 0 {
		t.Fatal("old run published a stale low-space result into new run")
	}
	m.checkStorage(oldStop)
	if len(m.StorageStatuses()) != 0 {
		t.Fatal("obsolete run published results")
	}
	m.checkStorage(m.cleanupStopCh)
	if paths := m.StorageStatuses(); len(paths) != 1 || !paths[0].Low {
		t.Fatalf("new run failed to recover monitoring: %+v", paths)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedUploadsRemainVisibleAfterRemovalAndRestart(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	m := storageTestManager(t, spool)
	cfg := testS3Recorder()
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	file := writeSpoolFile(t, spool, cfg.ID, "abandoned.mp3", "audio")
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/original.mp3", fileSize: 5}, firstAttempt: time.Now().Add(-25 * time.Hour)}
	if err := r.savePendingUpload(&p); err != nil {
		t.Fatal(err)
	}
	r.retryQueue = []pendingUpload{p}
	r.processRetryQueue()
	if m.PendingUploadCount() != 0 || m.AbandonedUploadCount() != 1 {
		t.Fatal("abandoned file was hidden or still queued for retry")
	}
	if err := m.RemoveRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	if m.AbandonedUploadCount() != 1 {
		t.Fatal("recorder removal hid abandoned audio")
	}
	m.checkStorage(m.cleanupStopCh)
	restarted := storageTestManager(t, spool)
	restarted.checkStorage(restarted.cleanupStopCh)
	if restarted.AbandonedUploadCount() != 1 {
		t.Fatal("startup scan hid abandoned audio of removed recorder")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "audio" { //nolint:gosec // Test recording.
		t.Fatalf("monitor changed recording: %q, %v", data, err)
	}
	// Re-adding its original configuration restores the existing retry policy,
	// without counting the same recording as both abandoned and pending.
	if err := restarted.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	restarted.checkStorage(restarted.cleanupStopCh)
	if restarted.AbandonedUploadCount() != 0 || restarted.PendingUploadCount() != 1 {
		t.Fatal("restart did not give abandoned upload a fresh retry window")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	m.checkStorage(m.cleanupStopCh)
	if m.AbandonedUploadCount() != 0 {
		t.Fatal("removed file still counted as retained audio")
	}
}
