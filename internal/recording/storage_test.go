package recording

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
		{"threshold inclusive", 1 << 30, 10 << 30, nil, false, 0},
		{"absolute threshold", (1 << 30) - 1, 10 << 30, nil, true, 1},
		{"still low", 0, 10 << 30, nil, true, 1},
		{"low until recovery margin", (2 << 30) - 1, 10 << 30, nil, true, 1},
		{"recovery margin inclusive", 2 << 30, 10 << 30, nil, false, 2},
		{"relative threshold", 2 << 30, 100 << 30, nil, true, 3},
		{"relative recovery margin", 7 << 30, 100 << 30, nil, true, 3},
		{"probe error is not recovery", 0, 0, errors.New("disk offline"), false, 3},
		// An unavailable path is not low: its next good probe uses the normal threshold.
		{"healthy again", 6 << 30, 100 << 30, nil, false, 4},
		// Small volumes recover halfway between the threshold and their size.
		{"small volume", (1 << 30) - 1, 3 << 29, nil, true, 5},
		{"small volume recovery attainable", 5 << 28, 3 << 29, nil, false, 6},
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
	if err != nil || len(events) != 14 {
		t.Fatalf("events=%+v err=%v; want two paths, six transitions and one change from low to unavailable", events, err)
	}
}

// runStorageSamples feeds available-byte samples for one local recorder path
// (the spool stays healthy) and counts alert callbacks and logged events.
func runStorageSamples(t *testing.T, total uint64, samples []uint64) (callbacks, low, restored int) {
	t.Helper()
	m := storageTestManager(t, t.TempDir())
	cfg := localOnDemandRecorder(t, "local")
	cfg.Enabled = true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	m.eventLogger = logger
	m.SetStorageChangeCallback(func([]types.RecordingStorageStatus) { callbacks++ })
	local := filepath.Clean(cfg.LocalPath)
	var available uint64
	m.diskSpace = func(p string) (uint64, uint64, error) {
		if p == local {
			return available, total, nil
		}
		return 10 << 30, 20 << 30, nil
	}
	for _, available = range samples {
		m.checkStorage(m.cleanupStopCh)
	}
	events, _, err := eventlog.ReadLast(logPath, eventlog.MaxReadLimit, 0, eventlog.FilterRecorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		switch e.Type {
		case eventlog.RecordingStorageLow:
			low++
		case eventlog.RecordingStorageRestored:
			restored++
		}
	}
	return callbacks, low, restored
}

func TestStorageOscillatingAroundThresholdAlertsOnce(t *testing.T) {
	t.Parallel()
	var samples []uint64
	for i := range 10 { // 4 MiB either side of the 1 GiB threshold.
		samples = append(samples, 1<<30-4<<20+uint64(i%2)*8<<20)
	}
	callbacks, low, restored := runStorageSamples(t, 16<<30, samples)
	if callbacks != 1 || low != 1 || restored != 0 {
		t.Fatalf("callbacks=%d low=%d restored=%d, want one low alert", callbacks, low, restored)
	}
}

// Hourly retention (or deletion after upload in S3-only mode) frees about an
// hour of audio at once, so free space near the threshold is a sawtooth.
func TestStorageRetentionSawtoothAlertsOnce(t *testing.T) {
	t.Parallel()
	const (
		total  = uint64(32_000_000_000) // 32 GB card: threshold 1.6 GB.
		perMin = uint64(15_000_000)     // PCM stored as S302M in MPEG-TS, ~0.9 GB per hour.
	)
	threshold := total / 20
	top := threshold + 30*perMin // Each hour straddles the threshold.
	var samples []uint64
	for range 6 {
		for minute := range uint64(60) {
			samples = append(samples, top-minute*perMin)
		}
	}
	callbacks, low, restored := runStorageSamples(t, total, samples)
	if callbacks != 1 || low != 1 || restored != 0 {
		t.Fatalf("callbacks=%d low=%d restored=%d, want one low alert", callbacks, low, restored)
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
	// A check while processRetryQueue has the queue drained must not count it.
	rr := restarted.recorders[cfg.ID]
	rr.mu.Lock()
	queued := rr.retryQueue
	rr.retryQueue = nil
	rr.mu.Unlock()
	restarted.checkStorage(restarted.cleanupStopCh)
	if restarted.AbandonedUploadCount() != 0 {
		t.Fatal("restored upload counted as abandoned while its retry ran")
	}
	rr.mu.Lock()
	rr.retryQueue = queued
	rr.mu.Unlock()
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

func TestUnreadableUploadMetadataDoesNotFailStorageCheck(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	m := storageTestManager(t, spool)
	dir := filepath.Join(spool, "uploads", "gone")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"+pendingUploadMetadataSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.checkStorage(m.cleanupStopCh)
	for _, p := range m.StorageStatuses() {
		if p.Error != "" {
			t.Fatalf("one unreadable metadata file failed %s: %s", p.Path, p.Error)
		}
	}
}

func TestRemovedLowPathClosesIncident(t *testing.T) {
	t.Parallel()
	m := storageTestManager(t, t.TempDir())
	cfg := localOnDemandRecorder(t, "local")
	cfg.Enabled = true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	m.eventLogger = logger
	m.diskSpace = func(p string) (uint64, uint64, error) {
		if p == filepath.Clean(cfg.LocalPath) {
			return 0, 10 << 30, nil
		}
		return 10 << 30, 20 << 30, nil
	}
	m.checkStorage(m.cleanupStopCh)
	if err := m.RemoveRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	m.checkStorage(m.cleanupStopCh)
	events, _, err := eventlog.ReadLast(logPath, 100, 0, eventlog.FilterRecorder)
	if err != nil {
		t.Fatal(err)
	}
	var low, restored int
	for _, e := range events {
		switch e.Type {
		case eventlog.RecordingStorageLow:
			low++
		case eventlog.RecordingStorageRestored:
			restored++
		}
	}
	if low != 1 || restored != 1 {
		t.Fatalf("removed low path incident: low=%d restored=%d, want 1 and 1", low, restored)
	}
}

func TestSpoolIsNotMonitoredWithoutRecorders(t *testing.T) {
	t.Parallel()
	m := storageTestManager(t, t.TempDir())
	m.diskSpace = func(string) (uint64, uint64, error) { return 0, 10 << 30, nil }
	m.checkStorage(m.cleanupStopCh)
	if got := m.StorageStatuses(); len(got) != 0 {
		t.Fatalf("an encoder without recorders reported recording storage: %+v", got)
	}
}

func TestFailedAbandonmentSaveKeepsUploadVisible(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions as a regular user")
	}
	spool := t.TempDir()
	m := storageTestManager(t, spool)
	cfg := testS3Recorder()
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	file := writeSpoolFile(t, spool, cfg.ID, "abandoned.mp3", "audio")
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/x.mp3", fileSize: 5}, firstAttempt: time.Now().Add(-25 * time.Hour)}
	if err := r.savePendingUpload(&p); err != nil {
		t.Fatal(err)
	}
	// A full or read-only spool cannot persist the abandonment itself.
	dir := filepath.Dir(p.metadataPath)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // A read-only test directory is the point.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore for cleanup.
	r.retryQueue = []pendingUpload{p}
	r.processRetryQueue()
	m.checkStorage(m.cleanupStopCh)
	if m.AbandonedUploadCount() != 1 {
		t.Fatal("an abandonment that could not be saved vanished from readiness")
	}
}

func TestDeletedAbandonedFileLeavesTheCount(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	m := storageTestManager(t, spool)
	cfg := testS3Recorder()
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	r := m.recorders[cfg.ID]
	file := writeSpoolFile(t, spool, cfg.ID, "abandoned.mp3", "audio")
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/x.mp3", fileSize: 5}, firstAttempt: time.Now().Add(-25 * time.Hour)}
	if err := r.savePendingUpload(&p); err != nil {
		t.Fatal(err)
	}
	r.retryQueue = []pendingUpload{p}
	r.processRetryQueue()
	if m.AbandonedUploadCount() != 1 {
		t.Fatal("abandonment not counted")
	}
	if err := os.Remove(file); err != nil { // Metadata still says abandoned.
		t.Fatal(err)
	}
	m.checkStorage(m.cleanupStopCh)
	if m.AbandonedUploadCount() != 0 {
		t.Fatal("a deleted abandoned file is still counted as retained audio")
	}
}

func TestScanDuringRestoreDoesNotResurrectAbandonment(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions as a regular user")
	}
	spool := t.TempDir()
	m := storageTestManager(t, spool)
	if err := m.AddRecorder(localOnDemandRecorder(t, "other")); err != nil { // Keeps the spool monitored.
		t.Fatal(err)
	}
	cfg := testS3Recorder()
	prior := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: spool})
	file := writeSpoolFile(t, spool, cfg.ID, "abandoned.mp3", "audio")
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/x.mp3", fileSize: 5}, firstAttempt: time.Now(), abandoned: true}
	if err := prior.savePendingUpload(&p); err != nil {
		t.Fatal(err)
	}
	// The restore cannot persist "no longer abandoned", so the scan still reads it.
	dir := filepath.Dir(p.metadataPath)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // A read-only test directory is the point.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore for cleanup.
	var restore sync.Once
	m.diskSpace = func(string) (uint64, uint64, error) {
		restore.Do(func() {
			if err := m.AddRecorder(cfg); err != nil {
				t.Error(err)
			}
		})
		return 10 << 30, 20 << 30, nil
	}
	m.checkStorage(m.cleanupStopCh)
	if m.AbandonedUploadCount() != 0 || m.PendingUploadCount() != 1 {
		t.Fatalf("restored upload: abandoned=%d pending=%d, want 0 and 1", m.AbandonedUploadCount(), m.PendingUploadCount())
	}
}

func TestRestartClosesStorageIncidentLeftOpen(t *testing.T) {
	type probe func(string) (uint64, uint64, error)
	space := func(available uint64) probe {
		return func(string) (uint64, uint64, error) { return available, 20 << 30, nil }
	}
	var (
		low     = space(0)
		margin  = space(3 << 29) // Above the 1 GiB threshold, below the 2 GiB recovery level.
		healthy = space(10 << 30)
		offline = probe(func(string) (uint64, uint64, error) { return 0, 0, errors.New("disk offline") })
	)
	for _, tt := range []struct {
		name    string
		history []probe // Observations before the restart; the incident stays open.
		buried  bool    // More unrelated events follow than one ReadLast page holds.
		flapped bool    // Another path then changes kind more often than one page holds.
		after   probe
		want    int // Callbacks on the first check after the restart.
	}{
		{"recovered", []probe{low}, false, false, healthy, 1},
		{"still low", []probe{low}, false, false, low, 0}, // No duplicate low.
		{"buried", []probe{low}, true, false, healthy, 1},
		{"low within recovery margin", []probe{low}, false, false, margin, 0},
		{"low behind another path's history", []probe{low}, false, true, margin, 0},
		{"unavailable within recovery margin", []probe{offline}, false, false, margin, 1},
		// The latest kind of problem decides, not the one that opened the incident.
		{"low then unavailable", []probe{low, offline}, false, false, margin, 1},
		{"unavailable then low", []probe{offline, low}, false, false, margin, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spool, logPath := t.TempDir(), filepath.Join(t.TempDir(), "events.jsonl")
			cfg := localOnDemandRecorder(t, "local")
			cfg.Enabled = true
			logger, err := eventlog.NewLogger(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = logger.Close() }()
			old := storageTestManager(t, spool)
			old.eventLogger = logger
			if err := old.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			alerts := 0
			old.SetStorageChangeCallback(func([]types.RecordingStorageStatus) { alerts++ })
			for _, p := range tt.history {
				old.diskSpace = p
				old.checkStorage(old.cleanupStopCh)
			}
			if alerts != 1 {
				t.Fatalf("alerts before restart = %d, want one (a change of kind is not a new alert)", alerts)
			}
			if tt.buried {
				for range 600 { // More than ReadLast's limit of unrelated events.
					_ = logger.Log(&eventlog.Event{Type: eventlog.RecorderStarted, Message: "unrelated"})
				}
			}
			if tt.flapped {
				for i := range eventlog.MaxReadLimit + 1 {
					other := types.RecordingStorageStatus{Path: "/other", Low: i%2 == 0}
					if !other.Low {
						other.Error = "offline"
					}
					_ = logger.Log(&eventlog.Event{Type: eventlog.RecordingStorageLow, Details: other})
				}
				_ = logger.Log(&eventlog.Event{Type: eventlog.RecordingStorageRestored, Details: types.RecordingStorageStatus{Path: "/other"}})
			}
			fresh, err := NewManager("", spool, 0, logger)
			if err != nil {
				t.Fatal(err)
			}
			fresh.running, fresh.cleanupStopCh = true, make(chan struct{})
			fresh.diskSpace = tt.after
			if err := fresh.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			callbacks := 0
			fresh.SetStorageChangeCallback(func([]types.RecordingStorageStatus) { callbacks++ })
			fresh.checkStorage(fresh.cleanupStopCh)
			if callbacks != tt.want {
				t.Fatalf("callbacks after restart = %d, want %d", callbacks, tt.want)
			}
			if tt.want == 0 {
				for _, p := range fresh.StorageStatuses() {
					if !p.Unhealthy() {
						t.Fatalf("open incident not restored: %+v", p)
					}
				}
				fresh.diskSpace = space(2 << 30)
				fresh.checkStorage(fresh.cleanupStopCh)
				if callbacks != 1 {
					t.Fatalf("callbacks after reaching the recovery level = %d, want one recovery", callbacks)
				}
			}
		})
	}
}

func TestFailedRegistrationKeepsUploadAbandoned(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	cfg := testS3Recorder()
	prior := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: spool})
	file := writeSpoolFile(t, spool, cfg.ID, "abandoned.mp3", "audio")
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/x.mp3", fileSize: 5}, firstAttempt: time.Now(), abandoned: true}
	if err := prior.savePendingUpload(&p); err != nil {
		t.Fatal(err)
	}
	// ReadDir sorts names, so this broken metadata fails reconciliation after
	// the abandoned entry was loaded.
	if err := os.Symlink(filepath.Join(spool, "missing"), filepath.Join(prior.pendingUploadDir(), "zzzz.upload.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := storageTestManager(t, spool)
	if err := m.AddRecorder(cfg); err == nil {
		t.Fatal("reconciliation unexpectedly succeeded")
	}
	m.checkStorage(m.cleanupStopCh)
	if got := m.AbandonedUploadCount(); got != 1 {
		t.Fatalf("abandoned count after a failed registration = %d, want 1", got)
	}
}
