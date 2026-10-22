package recording

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// lockReconcile takes a recorder's reconcile lock; tests replace it to see a
// call wait while another update or StopRecorder holds it.
var lockReconcile = (*sync.Mutex).Lock

// startHourlyRecorder is UpdateRecorder's start; tests replace it to hold that
// start.
var startHourlyRecorder = (*GenericRecorder).startHourly

// UploadBacklog counts recordings that are not delivered yet. Pending ones
// wait for or are in an upload; overdue ones (a subset) wait longer than two
// hours; failed and lost ones need the operator.
type UploadBacklog struct {
	Pending int
	Overdue int
	Failed  int
	Lost    int
}

// Manager coordinates multiple GenericRecorders.
type Manager struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex // Serializes Start/Stop without blocking status throughout teardown.

	recorders          map[string]*GenericRecorder
	removing           map[string]struct{}                // IDs whose removed recorder is still stopping.
	audioRecorders     atomic.Pointer[[]*GenericRecorder] // Immutable capture view; no I/O/management locks.
	spoolDir           string
	ffmpegPath         string
	maxDurationMinutes int  // Global max duration for on-demand recorders
	running            bool // Whether encoder is running (recorders should be active)
	eventLogger        *eventlog.Logger
	outbox             *outbox
	uploader           *uploader

	cleanupStopCh     chan struct{} // Stop signal for cleanup scheduler; created per run in Start, closed in Stop
	hourlyRetryStopCh chan struct{} // Stop signal for hourly retry scheduler; created per run in Start, closed in Stop

	storageProbeMu  sync.Mutex // Bounds non-cancellable filesystem probes across Stop/Start.
	storagePaths    []types.RecordingStorageStatus
	onStorageChange func([]types.RecordingStorageStatus)
	diskSpace       func(string) (uint64, uint64, error)
}

// NewManager creates a new recording manager.
func NewManager(ffmpegPath, spoolDir string, maxDurationMinutes int, eventLogger *eventlog.Logger) (*Manager, error) {
	if spoolDir == "" {
		spoolDir = DefaultSpoolDir()
	}

	// Ensure spool directory exists.
	// The spool directory needs to be readable. Its entry is synced also when
	// an earlier start created it but could not sync it.
	err := mkdirSynced(spoolDir, 0o755)
	if err == nil {
		err = syncDir(filepath.Dir(spoolDir))
	}
	if err != nil {
		return nil, fmt.Errorf("create recording spool directory: %w", err)
	}

	// A restart must still close storage incidents the previous run left open.
	var storagePaths []types.RecordingStorageStatus
	if eventLogger != nil {
		events, err := eventLogger.LatestStorageEvents()
		if err != nil {
			slog.Warn("could not restore recording storage incidents", "error", err)
		}
		for _, e := range events { // The latest event per path decides.
			if e.Type != eventlog.RecordingStorageLow {
				continue
			}
			details, _ := e.Details.(map[string]any)
			path, _ := details["path"].(string)
			// An unavailable path was not low; it recovers at the normal threshold.
			status := types.RecordingStorageStatus{Path: path, Low: true}
			if low, _ := details["low"].(bool); !low {
				errMsg, _ := details["error"].(string)
				status.Low, status.Error = false, cmp.Or(errMsg, "unavailable before restart")
			}
			storagePaths = append(storagePaths, status)
		}
	}

	ob, err := openOutbox(spoolDir, func(t *ticket) { logInterrupted(eventLogger, t) })
	if err != nil {
		return nil, err
	}

	m := &Manager{
		recorders:          make(map[string]*GenericRecorder),
		removing:           make(map[string]struct{}),
		spoolDir:           spoolDir,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		eventLogger:        eventLogger,
		outbox:             ob,
		uploader:           &uploader{outbox: ob, eventLogger: eventLogger, now: time.Now},
		storagePaths:       storagePaths,
		diskSpace:          diskSpace,
	}
	m.uploader.refresh = m.refreshDestinations
	return m, nil
}

func logInterrupted(eventLogger *eventlog.Logger, t *ticket) {
	slog.Warn("recovering a recording interrupted by a crash or power loss", "id", t.RecorderID, "file", t.Path)
	if eventLogger == nil {
		return
	}
	if err := eventLogger.LogRecorder(eventlog.RecorderError, &eventlog.RecorderDetails{
		RecorderName: t.RecorderName,
		Filename:     filepath.Base(t.Path),
		Error:        "recording was interrupted by a crash or power loss; kept up to that point",
	}); err != nil {
		slog.Warn("failed to log interrupted recording", "error", err)
	}
}

// SetUploadsOverdueCallback installs the handler for overdue uploads and their
// recovery; call it before StartUploads.
func (m *Manager) SetUploadsOverdueCallback(cb func(UploadsOverdue)) {
	m.uploader.mu.Lock()
	defer m.uploader.mu.Unlock()
	m.uploader.onOverdue = cb
}

// StartUploads starts delivering recordings, independent of the encoder's
// input: uploads continue while capture restarts. It is idempotent.
func (m *Manager) StartUploads() {
	open := false
	if m.eventLogger != nil {
		var err error
		if open, err = m.eventLogger.UploadOverdueOpen(); err != nil {
			slog.Warn("could not restore the upload overdue state", "error", err)
		}
	}
	m.uploader.start(open)
}

// Close stops delivering recordings and waits for an interrupted upload to be
// aborted. Call it after Stop and before closing the event log and alerts.
func (m *Manager) Close() {
	m.uploader.stop()
}

// UploadBacklog counts undelivered recordings, including those of removed
// recorders. A ticket that could not be read at startup counts as lost.
func (m *Manager) UploadBacklog() UploadBacklog {
	b := UploadBacklog{Lost: m.outbox.unreadable}
	now := m.uploader.clock()
	tickets := m.outbox.snapshot()
	for i := range tickets {
		t := &tickets[i]
		switch t.State {
		case ticketReady, ticketDelivered:
			b.Pending++
			if t.State == ticketReady && now.Sub(t.ReadyAt) >= uploadOverdueAfter {
				b.Overdue++
			}
		case ticketFailed:
			b.Failed++
		case ticketLost:
			b.Lost++
		}
	}
	return b
}

// saveDestination stores cfg's S3 credentials for its undelivered recordings,
// so a credential change reaches them and a removal does not strand them.
func (m *Manager) saveDestination(cfg *types.Recorder) error {
	if cfg.StorageMode == types.StorageLocal || !cfg.HasS3Credentials() {
		return nil
	}
	return m.outbox.saveDestination(destinationID(cfg.ID, cfg.S3Endpoint, cfg.S3Bucket), destinationOf(cfg))
}

// SetMaxDurationMinutes updates the maximum allowed duration for on-demand recordings.
// Affects new recordings only; recordings already in progress are not affected.
func (m *Manager) SetMaxDurationMinutes(minutes int) {
	m.mu.Lock()
	m.maxDurationMinutes = minutes
	recorders := slices.Collect(maps.Values(m.recorders))
	m.mu.Unlock()

	for _, r := range recorders {
		r.SetMaxDurationMinutes(minutes)
	}
}

// AddRecorder adds a recorder to the manager.
func (m *Manager) AddRecorder(cfg *types.Recorder) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.recorders[cfg.ID]; exists {
		return fmt.Errorf("recorder already exists: %s", cfg.ID)
	}
	// The removed run may still be finalizing into the same directory.
	if _, removing := m.removing[cfg.ID]; removing {
		return fmt.Errorf("recorder %s is still being removed", cfg.ID)
	}

	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder:           cfg,
		FFmpegPath:         m.ffmpegPath,
		SpoolDir:           m.spoolDir,
		MaxDurationMinutes: m.maxDurationMinutes,
		EventLogger:        m.eventLogger,
		Outbox:             m.outbox,
	})
	if err := m.saveDestination(cfg); err != nil {
		slog.Warn("cannot store upload destination; recording stores it again", "id", cfg.ID, "error", err)
	}
	m.recorders[cfg.ID] = recorder

	m.publishAudioRecordersLocked()

	// Auto-start hourly recorders if encoder is running (ondemand never auto-starts)
	if m.running {
		if err := recorder.startHourly(); err != nil {
			slog.Warn("failed to auto-start recorder", "id", cfg.ID, "error", err)
		}
	}

	slog.Info("recorder added", "id", cfg.ID, "name", cfg.Name)
	return nil
}

// RemoveRecorder removes a recorder from the manager.
func (m *Manager) RemoveRecorder(id string) error {
	m.mu.Lock()
	recorder, exists := m.recorders[id]
	if exists {
		delete(m.recorders, id)
		m.removing[id] = struct{}{}
		m.publishAudioRecordersLocked()
	}
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Delete before teardown so WriteAudio stops seeing this recorder.
	if err := recorder.Stop(); err != nil {
		slog.Warn("error stopping recorder during removal", "id", id, "error", err)
	}
	m.mu.Lock()
	delete(m.removing, id)
	m.mu.Unlock()

	slog.Info("recorder removed", "id", id)
	return nil
}

// UpdateRecorder updates an existing recorder configuration and applies its
// enabled state: a disabled recorder stops, and an enabled hourly recorder
// starts while the manager runs, also after an error the update may have fixed.
// A failed on-demand run is torn down. It waits for an earlier update or
// StopRecorder of this recorder, and a disabling or enabled hourly save also
// waits for a Stop in progress.
func (m *Manager) UpdateRecorder(cfg *types.Recorder) error {
	m.mu.RLock()
	recorder, exists := m.recorders[cfg.ID]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("recorder not found: %s", cfg.ID)
	}
	// A later update waits for this one, including its Stop, so it cannot
	// change the config between this one's config change and its stop or start.
	lockReconcile(&recorder.reconcileMu)
	defer recorder.reconcileMu.Unlock()
	if err := recorder.UpdateConfig(cfg); err != nil {
		return err
	}
	// Waiting recordings use the new credentials.
	if err := m.saveDestination(cfg); err != nil {
		return fmt.Errorf("store upload credentials: %w", err)
	}

	// Stop runs outside the manager lock, like RemoveRecorder. It also tears
	// down a failed run and waits for a Stop already in progress, so this
	// disable does not return early.
	if !cfg.Enabled {
		if err := recorder.Stop(); err != nil {
			slog.Warn("error stopping disabled recorder", "id", cfg.ID, "error", err)
		}
		return nil
	}
	if cfg.RecordingMode != types.RecordingHourly {
		// Saving clears a failed on-demand run, which only a start would reuse.
		if err := recorder.stopIn(types.ProcessError); err != nil {
			slog.Warn("error stopping failed recorder", "id", cfg.ID, "error", err)
		}
		return nil
	}
	// Wait for a Stop already in progress (such as the duration limit): it
	// would refuse the start below. A failed run is not stopped: startHourly
	// restarts it from ProcessError.
	_ = recorder.stopIn(types.ProcessStopping)

	// m.mu is held across the check and the start, so Manager.Stop and
	// RemoveRecorder either fail the check or see the recorder starting.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.running || m.recorders[cfg.ID] != recorder {
		return nil
	}
	if err := startHourlyRecorder(recorder); err != nil && !errors.Is(err, ErrAlreadyRecording) {
		slog.Warn("failed to start recorder after update", "id", cfg.ID, "error", err)
	}
	return nil
}

// StartRecorder starts an enabled on-demand recorder by ID.
func (m *Manager) StartRecorder(id string) error {
	// Held until the start returns, so Stop cannot pass between the running check and Start.
	m.mu.RLock()
	defer m.mu.RUnlock()

	recorder, exists := m.recorders[id]
	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}
	if !m.running {
		return ErrManagerNotRunning
	}
	return recorder.startOnDemand()
}

// StopRecorder stops an on-demand recorder by ID.
func (m *Manager) StopRecorder(id string) error {
	m.mu.RLock()
	recorder, exists := m.recorders[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Serialized with updates, so a save that switches the recorder to hourly
	// is applied after this stop instead of being undone by it.
	lockReconcile(&recorder.reconcileMu)
	defer recorder.reconcileMu.Unlock()

	// Only on-demand recorders can be stopped via API
	if recorder.Config().RecordingMode == types.RecordingHourly {
		return ErrHourlyRecorderNotControllable
	}

	// A start that waits for clock synchronization can be cancelled too.
	if state := recorder.Status().State; state != types.ProcessRunning && state != types.ProcessStarting {
		return ErrNotRecording
	}

	return recorder.Stop()
}

// Start begins recorder management and is idempotent across source retries.
func (m *Manager) Start() error {
	return m.StartIfActive(nil)
}

// StartIfActive checks source ownership under the same lock used by Stop.
// The predicate must not call this manager.
func (m *Manager) StartIfActive(active func() bool) error {
	m.StartUploads()
	m.lifecycleMu.Lock()
	m.mu.Lock()
	if active != nil && !active() {
		m.mu.Unlock()
		m.lifecycleMu.Unlock()
		return nil
	}

	if m.running {
		m.mu.Unlock()
		m.lifecycleMu.Unlock()
		return nil
	}
	m.running = true

	// Each scheduler captures its per-run stop channel.
	m.cleanupStopCh = make(chan struct{})
	m.hourlyRetryStopCh = make(chan struct{})

	// Start enabled hourly recorders (ondemand never auto-starts). The config
	// check is part of the start, as an update may change a recorder meanwhile.
	startErrors := make(map[string]error)
	for id, recorder := range m.recorders {
		if err := recorder.startHourly(); err != nil {
			startErrors[id] = err
		}
	}

	// Start cleanup scheduler
	m.startCleanupScheduler(m.cleanupStopCh)

	// Start hourly retry scheduler for failed recorders
	m.startHourlyRetryScheduler(m.hourlyRetryStopCh)
	go m.monitorStorage(m.cleanupStopCh)

	count := len(m.recorders)
	m.mu.Unlock()
	m.lifecycleMu.Unlock()
	for id, err := range startErrors {
		slog.Warn("failed to auto-start recorder", "id", id, "error", err)
	}
	slog.Info("recording manager started", "recorders", count)
	return nil
}

// IsRunning reports whether recorder management is active.
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// Stop stops all recorders and background schedulers.
func (m *Manager) Stop() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	// Close scheduler channels once; on-demand recorders may still need Stop.
	if m.running {
		m.running = false
		close(m.cleanupStopCh)
		close(m.hourlyRetryStopCh)
	}

	// Snapshot before blocking on FFmpeg shutdown.
	recorders := slices.Collect(maps.Values(m.recorders))
	m.mu.Unlock()

	// Stop recorders concurrently so one process cannot stall the rest.
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	for _, recorder := range recorders {
		wg.Go(func() {
			if err := recorder.Stop(); err != nil {
				slog.Warn("error stopping recorder", "id", recorder.ID(), "error", err)
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		})
	}
	wg.Wait()

	slog.Info("recording manager stopped")
	return errors.Join(errs...)
}

// WriteAudio fans out one PCM copy without blocking the distributor.
func (m *Manager) WriteAudio(pcm []byte) {
	var shared []byte
	m.WriteAudioShared(func() []byte {
		if shared == nil {
			shared = bytes.Clone(pcm)
		}
		return shared
	})
}

// WriteAudioShared queues one immutable chunk for every recording recorder,
// obtaining it from copyChunk only when a recorder is live. The caller may
// share the same copyChunk with the stream fan-out so one clone serves all
// consumers; recorders must treat queued chunks as read-only.
func (m *Manager) WriteAudioShared(copyChunk func() []byte) {
	recorders := m.audioRecorders.Load()
	if recorders == nil {
		return
	}
	for _, recorder := range *recorders {
		if !recorder.IsRecording() {
			continue
		}
		recorder.WriteAudio(copyChunk())
	}
}

// publishAudioRecordersLocked keeps capture independent of slow management operations.
func (m *Manager) publishAudioRecordersLocked() {
	recorders := slices.Collect(maps.Values(m.recorders))
	m.audioRecorders.Store(&recorders)
}

// Statuses returns the current status of all recorders.
func (m *Manager) Statuses() map[string]types.ProcessStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]types.ProcessStatus, len(m.recorders))
	for id, recorder := range m.recorders {
		statuses[id] = recorder.Status()
	}
	return statuses
}

// startHourlyRetryScheduler retries failed hourly recorders until stopCh
// closes. Capturing stopCh avoids racing Stop's field reset.
func (m *Manager) startHourlyRetryScheduler(stopCh <-chan struct{}) {
	util.RunHourly(stopCh, "recorder retry", m.retryFailedHourlyRecorders)
}

// refreshDestinations stores every recorder's current credentials again, so
// one whose save failed (at startup, say) recovers once storage does. The
// uploader runs it hourly, also while capture is stopped.
func (m *Manager) refreshDestinations() {
	m.mu.RLock()
	recorders := slices.Collect(maps.Values(m.recorders))
	m.mu.RUnlock()
	for _, r := range recorders {
		// Serialized with updates, so it never writes an older config.
		lockReconcile(&r.reconcileMu)
		cfg := r.Config()
		if err := m.saveDestination(&cfg); err != nil {
			slog.Warn("cannot store upload destination", "id", cfg.ID, "error", err)
		}
		r.reconcileMu.Unlock()
	}
}

func (m *Manager) retryFailedHourlyRecorders() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.running {
		return
	}

	for _, recorder := range m.recorders {
		cfg := recorder.Config()
		if cfg.RecordingMode == types.RecordingHourly && cfg.Enabled {
			status := recorder.Status()
			if status.State == types.ProcessError {
				slog.Info("retrying failed hourly recorder at hour boundary", "id", cfg.ID, "name", cfg.Name)
				// An update may have changed it since the check above.
				if err := recorder.startHourly(); err != nil {
					slog.Warn("failed to retry hourly recorder", "id", recorder.ID(), "error", err)
				}
			}
		}
	}
}
