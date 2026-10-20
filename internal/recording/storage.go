package recording

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const storageCheckInterval = time.Minute

// SetStorageChangeCallback installs an alert handler before Start. The handler
// receives all current path observations whenever the set of unhealthy paths changes.
func (m *Manager) SetStorageChangeCallback(cb func([]types.RecordingStorageStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStorageChange = cb
}

func (m *Manager) handleUploadAbandoned(event *UploadAbandonedEvent) {
	m.mu.Lock()
	m.abandoned[event.LocalPath] = struct{}{}
	cb := m.onUploadAbandoned
	m.mu.Unlock()
	if cb != nil {
		cb(*event)
	}
}

// AbandonedUploadCount includes retained uploads of removed recorders. It does
// not re-queue them; missing files are reconciled by the background disk check.
func (m *Manager) AbandonedUploadCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.abandoned)
}

// StorageStatuses returns cached observations without accessing recording disks.
func (m *Manager) StorageStatuses() []types.RecordingStorageStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.storagePaths)
}

func (m *Manager) monitorStorage(stop <-chan struct{}) {
	ticker := time.NewTicker(storageCheckInterval)
	defer ticker.Stop()
	for {
		m.checkStorage(stop)
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) checkStorage(stop <-chan struct{}) {
	// OS disk probes cannot be cancelled; allow only one in flight across restarts.
	if !m.storageProbeMu.TryLock() {
		return
	}
	defer m.storageProbeMu.Unlock()
	ordered, activeUploads, known, gen := m.storageTargets()
	observations := m.probeStorage(ordered)
	// Scan retained metadata too: removing a recorder must not hide its abandoned
	// audio after a process restart. This scan never modifies metadata or audio.
	abandoned, scanErr := scanAbandonedUploads(m.spoolDir, activeUploads)
	if scanErr != nil {
		i := slices.IndexFunc(observations, func(o types.RecordingStorageStatus) bool {
			return o.Path == filepath.Clean(m.spoolDir)
		})
		if i < 0 {
			observations = append(observations, types.RecordingStorageStatus{Path: filepath.Clean(m.spoolDir), CheckedAt: time.Now()})
			i = len(observations) - 1
		}
		msg := "scan abandoned uploads: " + scanErr.Error()
		if observations[i].Error != "" {
			msg = observations[i].Error + "; " + msg
		}
		observations[i].Error = msg
	}
	var missing []string
	for _, path := range known {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, path)
		}
	}

	m.mu.Lock()
	if !m.running || m.cleanupStopCh != stop {
		m.mu.Unlock()
		return // An old, blocked run must not publish into a new run.
	}
	previous := m.storagePaths
	m.storagePaths = observations
	// Add, never replace: an abandonment the callback reported after the scan
	// started is not on disk yet. Restored uploads are saved as not abandoned.
	if m.restoreGen == gen { // A restore during the scan makes its result stale.
		maps.Copy(m.abandoned, abandoned)
	}
	for _, path := range missing {
		delete(m.abandoned, path)
	}
	cb := m.onStorageChange
	m.mu.Unlock()
	// Logging and delivery run outside status locks (and never on capture).
	if m.logStorageChanges(previous, observations) && cb != nil {
		cb(slices.Clone(observations))
	}
}

func (m *Manager) storageTargets() (ordered []string, activeUploads map[string]struct{}, abandoned []string, restoreGen uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	paths := make(map[string]struct{})
	// An encoder that does not record has no spool to fill.
	if len(m.recorders) > 0 || len(m.abandoned) > 0 {
		paths[filepath.Clean(m.spoolDir)] = struct{}{}
	}
	activeUploads = make(map[string]struct{})
	for _, r := range m.recorders {
		cfg := r.Config()
		if (cfg.Enabled || r.IsRecording()) && cfg.StorageMode != types.StorageS3 && cfg.LocalPath != "" {
			paths[filepath.Clean(cfg.LocalPath)] = struct{}{}
		}
		r.mu.RLock()
		for i := range r.retryQueue {
			activeUploads[r.retryQueue[i].request.localPath] = struct{}{}
		}
		r.mu.RUnlock()
	}
	return slices.Sorted(maps.Keys(paths)), activeUploads, slices.Collect(maps.Keys(m.abandoned)), m.restoreGen
}

func (m *Manager) probeStorage(ordered []string) []types.RecordingStorageStatus {
	observations := make([]types.RecordingStorageStatus, 0, len(ordered))
	for _, path := range ordered {
		available, total, err := m.diskSpace(path)
		status := types.RecordingStorageStatus{
			Path: path, AvailableBytes: available, TotalBytes: total,
			ThresholdBytes: max(1<<30, total/20), CheckedAt: time.Now(),
		}
		switch {
		case err != nil:
			status.Error = err.Error()
		case total == 0 || available > total:
			status.Error = "invalid filesystem space counts"
		default:
			status.Low = available < status.ThresholdBytes
		}
		observations = append(observations, status)
	}
	return observations
}

// logStorageChanges logs per-path health transitions, including paths that left
// monitoring while unhealthy, and reports whether any transition happened.
func (m *Manager) logStorageChanges(previous, current []types.RecordingStorageStatus) (changed bool) {
	was := make(map[string]types.RecordingStorageStatus, len(previous))
	for _, p := range previous {
		was[p.Path] = p
	}
	transitions := make([]types.RecordingStorageStatus, 0)
	for _, p := range current {
		old := was[p.Path]
		delete(was, p.Path)
		if p.Unhealthy() != old.Unhealthy() {
			transitions = append(transitions, p)
		}
	}
	for _, old := range was {
		if old.Unhealthy() {
			transitions = append(transitions, types.RecordingStorageStatus{Path: old.Path, CheckedAt: time.Now()})
		}
	}
	for _, p := range transitions {
		t, message := eventlog.RecordingStorageRestored, "Recording storage recovered"
		if p.Unhealthy() {
			t, message = eventlog.RecordingStorageLow, "Recording storage is low or unavailable"
			slog.Warn(message, "path", p.Path, "available_bytes", p.AvailableBytes, "error", p.Error)
		} else {
			slog.Info(message, "path", p.Path, "available_bytes", p.AvailableBytes)
		}
		if m.eventLogger != nil {
			if err := m.eventLogger.Log(&eventlog.Event{Type: t, Message: message, Details: p}); err != nil {
				slog.Warn("failed to log recording storage event", "error", err)
			}
		}
	}
	return len(transitions) > 0
}

func scanAbandonedUploads(spool string, active map[string]struct{}) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	root := filepath.Join(spool, "uploads")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), pendingUploadMetadataSuffix) {
			return nil
		}
		// One unreadable file (a finished retry removing it, or a power cut
		// mid-write) must not fail the spool check; it holds no abandonment.
		data, err := os.ReadFile(path) //nolint:gosec // Metadata in the manager-owned spool; read-only.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		var meta pendingUploadMetadata
		if err == nil {
			err = json.Unmarshal(data, &meta)
		}
		if err != nil {
			slog.Warn("skipping unreadable upload metadata", "path", path, "error", err)
			return nil
		}
		if _, queued := active[meta.LocalPath]; !queued && meta.Abandoned && meta.LocalPath != "" {
			if _, err := os.Stat(meta.LocalPath); err == nil {
				found[meta.LocalPath] = struct{}{}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
	return found, err
}
