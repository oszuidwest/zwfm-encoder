package recording

import (
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
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
	observations := m.probeStorage(m.storageTargets())

	m.mu.Lock()
	if !m.running || m.cleanupStopCh != stop {
		m.mu.Unlock()
		return // An old, blocked run must not publish into a new run.
	}
	previous := m.storagePaths
	holdLowUntilRecovered(previous, observations)
	m.storagePaths = observations
	cb := m.onStorageChange
	m.mu.Unlock()
	// Logging and delivery run outside status locks (and never on capture).
	if m.logStorageChanges(previous, observations) && cb != nil {
		cb(slices.Clone(observations))
	}
}

// storageTargets lists the spool and every directory that holds recordings:
// those of enabled or recording recorders, and those of undelivered or failed
// recordings, also of removed recorders.
func (m *Manager) storageTargets() []string {
	paths := make(map[string]struct{})
	tickets := m.outbox.snapshot()
	spool := filepath.Clean(m.spoolDir)
	for i := range tickets {
		dir := filepath.Clean(filepath.Dir(tickets[i].Path))
		if rel, err := filepath.Rel(spool, dir); err == nil && filepath.IsLocal(rel) {
			dir = spool // The spool is one filesystem.
		}
		paths[dir] = struct{}{}
	}
	m.mu.RLock()
	// An encoder that does not record has no spool to fill.
	if len(m.recorders) > 0 || len(tickets) > 0 {
		paths[spool] = struct{}{}
	}
	for _, r := range m.recorders {
		cfg := r.Config()
		if (cfg.Enabled || r.IsRecording()) && cfg.StorageMode != types.StorageS3 && cfg.LocalPath != "" {
			paths[filepath.Clean(cfg.LocalPath)] = struct{}{}
		}
	}
	m.mu.RUnlock()
	return slices.Sorted(maps.Keys(paths))
}

func (m *Manager) probeStorage(ordered []string) []types.RecordingStorageStatus {
	observations := make([]types.RecordingStorageStatus, 0, len(ordered))
	for _, path := range ordered {
		available, total, err := m.diskSpace(path)
		threshold := max(1<<30, total/20)
		status := types.RecordingStorageStatus{
			Path: path, AvailableBytes: available, TotalBytes: total,
			ThresholdBytes: threshold, RecoveryBytes: threshold + recoveryMargin(threshold, total), CheckedAt: time.Now(),
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

// recoveryMargin is the extra free space a low path needs to recover. Near the
// threshold free space is an hourly sawtooth: recordings use it up and hourly
// retention (or deletion after upload in S3-only mode) frees about an hour of
// audio at once. From 3 GiB up, max(1 GiB, 2.5%) absorbs an hour of one PCM
// recorder (~0.9 GB stored) or several MP3 recorders (~0.15 GB each); larger
// swings can still alert hourly. Below 3 GiB the margin is capped at half the
// space between the threshold and the total so a low path can still recover; a
// volume smaller than the threshold gets no margin and stays low.
func recoveryMargin(threshold, total uint64) uint64 {
	if total <= threshold {
		return 0
	}
	return min(max(1<<30, total/40), (total-threshold)/2)
}

// holdLowUntilRecovered keeps a path that was low or unavailable low until it
// has RecoveryBytes free, so a failed probe cannot end a low incident early.
func holdLowUntilRecovered(previous, current []types.RecordingStorageStatus) {
	for i := range current {
		p := &current[i]
		if p.Low || p.Error != "" || p.AvailableBytes >= p.RecoveryBytes {
			continue
		}
		p.Low = slices.ContainsFunc(previous, func(old types.RecordingStorageStatus) bool {
			return old.Path == p.Path && old.Unhealthy()
		})
	}
}

// logStorageChanges logs per-path health transitions, including paths that left
// monitoring while unhealthy and switches between low and unavailable (the
// operator then has to act differently), and reports whether any happened.
func (m *Manager) logStorageChanges(previous, current []types.RecordingStorageStatus) bool {
	was := make(map[string]types.RecordingStorageStatus, len(previous))
	for _, p := range previous {
		was[p.Path] = p
	}
	logged := make([]types.RecordingStorageStatus, 0)
	for _, p := range current {
		old := was[p.Path]
		delete(was, p.Path)
		if p.Unhealthy() != old.Unhealthy() || p.Unhealthy() && p.Low != old.Low {
			logged = append(logged, p)
		}
	}
	for _, old := range was {
		if old.Unhealthy() {
			logged = append(logged, types.RecordingStorageStatus{Path: old.Path, CheckedAt: time.Now()})
		}
	}
	for _, p := range logged {
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
	return len(logged) > 0
}
