package recording

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// UploadAbandonedEvent contains details about an upload that was abandoned after exhausting retries.
type UploadAbandonedEvent struct {
	RecorderName string
	Filename     string
	S3Key        string
	LastError    string
	RetryCount   int
}

// UploadAbandonedCallback is called when an upload is abandoned after exceeding the retry limit.
type UploadAbandonedCallback func(UploadAbandonedEvent)

// Manager coordinates multiple GenericRecorders.
type Manager struct {
	mu sync.RWMutex

	recorders          map[string]*GenericRecorder
	spoolDir           string
	ffmpegPath         string
	maxDurationMinutes int  // Global max duration for on-demand recorders
	running            bool // Whether encoder is running (recorders should be active)
	eventLogger        *eventlog.Logger
	onUploadAbandoned  UploadAbandonedCallback

	cleanupStopCh     chan struct{} // Stop signal for cleanup scheduler; created per run in Start, closed in Stop
	hourlyRetryStopCh chan struct{} // Stop signal for hourly retry scheduler; created per run in Start, closed in Stop
}

// NewManager creates a new recording manager.
func NewManager(ffmpegPath, spoolDir string, maxDurationMinutes int, eventLogger *eventlog.Logger) (*Manager, error) {
	if spoolDir == "" {
		spoolDir = DefaultSpoolDir()
	}

	// Ensure spool directory exists.
	if err := os.MkdirAll(spoolDir, 0o755); err != nil { //nolint:gosec // Spool directory needs to be readable
		return nil, fmt.Errorf("create recording spool directory: %w", err)
	}

	return &Manager{
		recorders:          make(map[string]*GenericRecorder),
		spoolDir:           spoolDir,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		eventLogger:        eventLogger,
	}, nil
}

// SetUploadAbandonedCallback configures the handler called when an upload is abandoned.
func (m *Manager) SetUploadAbandonedCallback(cb UploadAbandonedCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onUploadAbandoned = cb
}

// SetMaxDurationMinutes updates the maximum allowed duration for on-demand recordings.
// Affects new recordings only; recordings already in progress are not affected.
func (m *Manager) SetMaxDurationMinutes(minutes int) {
	m.mu.Lock()
	m.maxDurationMinutes = minutes
	recorders := make([]*GenericRecorder, 0, len(m.recorders))
	for _, r := range m.recorders {
		recorders = append(recorders, r)
	}
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

	recorder, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder:           cfg,
		FFmpegPath:         m.ffmpegPath,
		SpoolDir:           m.spoolDir,
		MaxDurationMinutes: m.maxDurationMinutes,
		EventLogger:        m.eventLogger,
		OnUploadAbandoned:  m.onUploadAbandoned,
	})
	if err != nil {
		return fmt.Errorf("create recorder: %w", err)
	}
	if err := recorder.reconcilePendingUploads(); err != nil {
		return fmt.Errorf("reconcile pending uploads: %w", err)
	}

	m.recorders[cfg.ID] = recorder

	// Auto-start hourly recorders if encoder is running (ondemand never auto-starts)
	if m.running && cfg.RecordingMode == types.RecordingHourly && cfg.Enabled {
		if err := recorder.Start(); err != nil {
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
	}
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Delete before teardown so WriteAudio stops seeing this recorder.
	if err := recorder.Stop(); err != nil {
		slog.Warn("error stopping recorder during removal", "id", id, "error", err)
	}

	slog.Info("recorder removed", "id", id)
	return nil
}

// UpdateRecorder updates an existing recorder configuration.
func (m *Manager) UpdateRecorder(cfg *types.Recorder) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	recorder, exists := m.recorders[cfg.ID]
	if !exists {
		return fmt.Errorf("recorder not found: %s", cfg.ID)
	}

	return recorder.UpdateConfig(cfg)
}

// StartRecorder starts an on-demand recorder by ID.
func (m *Manager) StartRecorder(id string) error {
	m.mu.RLock()
	recorder, exists := m.recorders[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Only on-demand recorders can be started via API
	if recorder.Config().RecordingMode == types.RecordingHourly {
		return ErrHourlyRecorderNotControllable
	}

	// Check if already recording
	if recorder.IsRecording() {
		return ErrAlreadyRecording
	}

	return recorder.Start()
}

// StopRecorder stops an on-demand recorder by ID.
func (m *Manager) StopRecorder(id string) error {
	m.mu.RLock()
	recorder, exists := m.recorders[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Only on-demand recorders can be stopped via API
	if recorder.Config().RecordingMode == types.RecordingHourly {
		return ErrHourlyRecorderNotControllable
	}

	// Check if not recording
	if !recorder.IsRecording() {
		return ErrNotRecording
	}

	return recorder.Stop()
}

// Start begins recorder management and is idempotent across source retries.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}
	m.running = true

	// Each scheduler captures its per-run stop channel.
	m.cleanupStopCh = make(chan struct{})
	m.hourlyRetryStopCh = make(chan struct{})

	// Start hourly recorders (ondemand never auto-starts)
	for id, recorder := range m.recorders {
		cfg := recorder.Config()
		if cfg.RecordingMode == types.RecordingHourly && cfg.Enabled {
			if err := recorder.Start(); err != nil {
				slog.Warn("failed to auto-start recorder", "id", id, "error", err)
			}
		}
	}

	// Start cleanup scheduler
	m.startCleanupScheduler(m.cleanupStopCh)

	// Start hourly retry scheduler for failed recorders
	m.startHourlyRetryScheduler(m.hourlyRetryStopCh)
	go m.processUploadRetryQueues()

	slog.Info("recording manager started", "recorders", len(m.recorders))
	return nil
}

// Stop stops all recorders and background schedulers.
func (m *Manager) Stop() error {
	m.mu.Lock()
	// Close scheduler channels once; on-demand recorders may still need Stop.
	if m.running {
		m.running = false
		close(m.cleanupStopCh)
		close(m.hourlyRetryStopCh)
	}

	// Snapshot before blocking on FFmpeg shutdown.
	recorders := make([]*GenericRecorder, 0, len(m.recorders))
	for _, recorder := range m.recorders {
		recorders = append(recorders, recorder)
	}
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
	m.mu.RLock()
	defer m.mu.RUnlock()

	var shared []byte
	for _, recorder := range m.recorders {
		if !recorder.IsRecording() {
			continue
		}
		if shared == nil {
			shared = make([]byte, len(pcm))
			copy(shared, pcm)
		}
		recorder.WriteAudio(shared)
	}
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

// PendingUploadCount returns the total number of uploads waiting for retry.
func (m *Manager) PendingUploadCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, recorder := range m.recorders {
		count += recorder.PendingUploadCount()
	}
	return count
}

// startHourlyRetryScheduler retries hourly recorders and uploads until stopCh closes.
// Capturing stopCh avoids racing Stop's field reset.
func (m *Manager) startHourlyRetryScheduler(stopCh <-chan struct{}) {
	go func() {
		for {
			duration := util.TimeUntilNextHour(time.Now())
			timer := time.NewTimer(duration)
			select {
			case <-stopCh:
				timer.Stop()
				return
			case <-timer.C:
				m.retryFailedHourlyRecorders()
				m.processUploadRetryQueues()
			}
		}
	}()
}

func (m *Manager) retryFailedHourlyRecorders() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, recorder := range m.recorders {
		cfg := recorder.Config()
		if cfg.RecordingMode == types.RecordingHourly && cfg.Enabled {
			status := recorder.Status()
			if status.State == types.ProcessError {
				slog.Info("retrying failed hourly recorder at hour boundary", "id", cfg.ID, "name", cfg.Name)
				go func(r *GenericRecorder) {
					if err := r.Start(); err != nil {
						slog.Warn("failed to retry hourly recorder", "id", r.ID(), "error", err)
					}
				}(recorder)
			}
		}
	}
}

func (m *Manager) processUploadRetryQueues() {
	m.mu.RLock()
	recorders := make([]*GenericRecorder, 0, len(m.recorders))
	for _, recorder := range m.recorders {
		recorders = append(recorders, recorder)
	}
	m.mu.RUnlock()

	for _, recorder := range recorders {
		recorder.processRetryQueue()
	}
}
