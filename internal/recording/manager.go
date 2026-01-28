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

// Manager coordinates multiple GenericRecorders.
type Manager struct {
	mu sync.RWMutex

	recorders          map[string]*GenericRecorder
	tempDir            string
	ffmpegPath         string
	maxDurationMinutes int  // Global max duration for on-demand recorders
	running            bool // Whether encoder is running (recorders should be active)
	eventLogger        *eventlog.Logger

	cleanupStopCh     chan struct{} // Stop signal for cleanup scheduler
	hourlyRetryStopCh chan struct{} // Stop signal for hourly retry scheduler
}

// NewManager creates a new recording manager.
func NewManager(ffmpegPath, tempDir string, maxDurationMinutes int, eventLogger *eventlog.Logger) (*Manager, error) {
	if tempDir == "" {
		tempDir = DefaultTempDir
	}

	// Ensure temp directory exists
	if err := os.MkdirAll(tempDir, 0o755); err != nil { //nolint:gosec // Temp directory needs to be readable
		return nil, fmt.Errorf("create temp directory: %w", err)
	}

	return &Manager{
		recorders:          make(map[string]*GenericRecorder),
		tempDir:            tempDir,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		eventLogger:        eventLogger,
		cleanupStopCh:      make(chan struct{}),
		hourlyRetryStopCh:  make(chan struct{}),
	}, nil
}

// AddRecorder adds a recorder to the manager.
func (m *Manager) AddRecorder(cfg *types.Recorder) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.recorders[cfg.ID]; exists {
		return fmt.Errorf("recorder already exists: %s", cfg.ID)
	}

	recorder, err := NewGenericRecorder(cfg, m.ffmpegPath, m.tempDir, m.maxDurationMinutes, m.eventLogger)
	if err != nil {
		return fmt.Errorf("create recorder: %w", err)
	}

	m.recorders[cfg.ID] = recorder

	// Auto-start hourly recorders if encoder is running (ondemand never auto-starts)
	if m.running && cfg.RotationMode == types.RotationHourly && cfg.IsEnabled() {
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
	defer m.mu.Unlock()

	recorder, exists := m.recorders[id]
	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Always stop to clean up resources (timers, upload worker) even in error state
	if err := recorder.Stop(); err != nil {
		slog.Warn("error stopping recorder during removal", "id", id, "error", err)
	}

	delete(m.recorders, id)
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
	if recorder.Config().RotationMode == types.RotationHourly {
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
	if recorder.Config().RotationMode == types.RotationHourly {
		return ErrHourlyRecorderNotControllable
	}

	// Check if not recording
	if !recorder.IsRecording() {
		return ErrNotRecording
	}

	return recorder.Stop()
}

// Start begins recorder management, starting hourly recorders and cleanup schedulers.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.running = true

	// Start hourly recorders (ondemand never auto-starts)
	for id, recorder := range m.recorders {
		cfg := recorder.Config()
		if cfg.RotationMode == types.RotationHourly && cfg.IsEnabled() {
			if err := recorder.Start(); err != nil {
				slog.Warn("failed to auto-start recorder", "id", id, "error", err)
			}
		}
	}

	// Start cleanup scheduler
	m.startCleanupScheduler()

	// Start hourly retry scheduler for failed recorders
	m.startHourlyRetryScheduler()

	slog.Info("recording manager started", "recorders", len(m.recorders))
	return nil
}

// Stop stops all active recorders and background schedulers.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.running = false

	// Stop cleanup scheduler
	close(m.cleanupStopCh)
	m.cleanupStopCh = make(chan struct{}) // Reset for potential restart

	// Stop hourly retry scheduler
	close(m.hourlyRetryStopCh)
	m.hourlyRetryStopCh = make(chan struct{}) // Reset for potential restart

	var errs []error
	for id, recorder := range m.recorders {
		if recorder.IsRecording() {
			if err := recorder.Stop(); err != nil {
				slog.Warn("error stopping recorder", "id", id, "error", err)
				errs = append(errs, err)
			}
		}
	}

	slog.Info("recording manager stopped")
	return errors.Join(errs...)
}

// WriteAudio writes PCM audio to any active recorders.
func (m *Manager) WriteAudio(pcm []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, recorder := range m.recorders {
		if recorder.IsRecording() {
			if err := recorder.WriteAudio(pcm); err != nil {
				slog.Warn("recorder write error", "id", recorder.ID(), "error", err)
			}
		}
	}

	return nil
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

// startHourlyRetryScheduler retries failed hourly recorders at each hour boundary.
func (m *Manager) startHourlyRetryScheduler() {
	go func() {
		for {
			duration := util.TimeUntilNextHour(time.Now())
			select {
			case <-m.hourlyRetryStopCh:
				return
			case <-time.After(duration):
				m.retryFailedHourlyRecorders()
			}
		}
	}()
}

func (m *Manager) retryFailedHourlyRecorders() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, recorder := range m.recorders {
		cfg := recorder.Config()
		if cfg.RotationMode == types.RotationHourly && cfg.IsEnabled() {
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
