package recording

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	m.onStorageChange = cb
}

func (m *Manager) handleUploadAbandoned(event *UploadAbandonedEvent) {
	m.storageMu.Lock()
	m.abandoned[event.LocalPath] = struct{}{}
	m.storageMu.Unlock()
	m.mu.RLock()
	cb := m.onUploadAbandoned
	m.mu.RUnlock()
	if cb != nil {
		cb(*event)
	}
}

// AbandonedUploadCount includes retained uploads of removed recorders. It does
// not re-queue them; missing files are reconciled by the background disk check.
func (m *Manager) AbandonedUploadCount() int {
	m.storageMu.RLock()
	defer m.storageMu.RUnlock()
	return len(m.abandoned)
}

// StorageStatuses returns cached observations without accessing recording disks.
func (m *Manager) StorageStatuses() []types.RecordingStorageStatus {
	m.storageMu.RLock()
	defer m.storageMu.RUnlock()
	return slices.Clone(m.storagePaths)
}

func (m *Manager) monitorStorage(stop <-chan struct{}) {
	ticker := time.NewTicker(storageCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		m.checkStorage(stop)
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) checkStorage(stop <-chan struct{}) {
	// shortcut: OS disk probes cannot be cancelled. Keep at most one in flight,
	// even across restarts; isolate in a helper process if bounded probe exit is required.
	if !m.storageProbeMu.TryLock() {
		return
	}
	defer m.storageProbeMu.Unlock()
	ordered, activeUploads := m.storageTargets()
	observations := m.probeStorage(ordered)
	// Scan retained metadata too: removing a recorder must not hide its abandoned
	// audio after a process restart. This scan never modifies metadata or audio.
	abandoned, scanErr := scanAbandonedUploads(m.spoolDir, activeUploads)
	if scanErr != nil {
		for i := range observations {
			if observations[i].Path == filepath.Clean(m.spoolDir) {
				observations[i].Error = strings.TrimPrefix(fmt.Sprintf("%s; scan abandoned uploads: %v", observations[i].Error, scanErr), "; ")
			}
		}
	}
	m.storageMu.RLock()
	known := make([]string, 0, len(m.abandoned))
	for path := range m.abandoned {
		known = append(known, path)
	}
	m.storageMu.RUnlock()
	missing := make([]string, 0)
	for _, path := range known {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, path)
		}
	}

	m.mu.RLock()
	if !m.running || m.cleanupStopCh != stop {
		m.mu.RUnlock()
		return // An old, blocked run must not publish into a new run.
	}
	m.storageMu.Lock()
	previous := m.storagePaths
	m.storagePaths = observations
	for path := range abandoned {
		m.abandoned[path] = struct{}{}
	}
	for _, path := range missing {
		delete(m.abandoned, path)
	}
	cb := m.onStorageChange
	m.storageMu.Unlock()
	m.mu.RUnlock()
	// Logging and delivery run outside status locks (and never on capture).
	m.logStorageChanges(previous, observations)
	if cb != nil && !slices.Equal(storageProblems(previous), storageProblems(observations)) {
		cb(slices.Clone(observations))
	}
}

func (m *Manager) storageTargets() (ordered []string, activeUploads map[string]struct{}) {
	m.mu.RLock()
	paths := map[string]struct{}{filepath.Clean(m.spoolDir): {}}
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
	m.mu.RUnlock()
	ordered = make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	slices.Sort(ordered)
	return ordered, activeUploads
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

func storageProblems(paths []types.RecordingStorageStatus) []string {
	var problems []string
	for _, p := range paths {
		if p.Low || p.Error != "" {
			problems = append(problems, p.Path)
		}
	}
	return problems
}

func (m *Manager) logStorageChanges(previous, current []types.RecordingStorageStatus) {
	for _, p := range current {
		oldProblem := false
		for _, old := range previous {
			if old.Path == p.Path {
				oldProblem = old.Low || old.Error != ""
			}
		}
		if problem := p.Low || p.Error != ""; problem != oldProblem {
			t := eventlog.RecordingStorageLow
			message := "Recording storage is low or unavailable"
			if !problem {
				t, message = eventlog.RecordingStorageRestored, "Recording storage recovered"
			}
			if m.eventLogger != nil {
				if err := m.eventLogger.Log(&eventlog.Event{Type: t, Message: message, Details: p}); err != nil {
					slog.Warn("failed to log recording storage event", "error", err)
				}
			}
			if problem {
				slog.Warn(message, "path", p.Path, "available_bytes", p.AvailableBytes, "error", p.Error)
			} else {
				slog.Info(message, "path", p.Path, "available_bytes", p.AvailableBytes)
			}
		}
	}
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
		data, err := os.ReadFile(path) //nolint:gosec // Metadata in the manager-owned spool; read-only.
		if err != nil {
			return err
		}
		var meta pendingUploadMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
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
