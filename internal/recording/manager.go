package recording

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// Manager orchestrates multiple GenericRecorders.
type Manager struct {
	mu sync.RWMutex

	recorders          map[string]*GenericRecorder
	apiKey             string
	tempDir            string
	ffmpegPath         string
	maxDurationMinutes int  // Global max duration for on-demand recorders
	running            bool // Whether encoder is running (recorders should be active)

	cleanupStopCh chan struct{} // Stop signal for cleanup scheduler
}

// NewManager creates a new recording manager.
// ffmpegPath is the path to the FFmpeg binary.
// maxDurationMinutes is the global max duration for on-demand recorders.
func NewManager(ffmpegPath, apiKey, tempDir string, maxDurationMinutes int) (*Manager, error) {
	if tempDir == "" {
		tempDir = DefaultTempDir
	}

	// Ensure temp directory exists
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("create temp directory: %w", err)
	}

	return &Manager{
		recorders:          make(map[string]*GenericRecorder),
		apiKey:             apiKey,
		tempDir:            tempDir,
		ffmpegPath:         ffmpegPath,
		maxDurationMinutes: maxDurationMinutes,
		cleanupStopCh:      make(chan struct{}),
	}, nil
}

// AddRecorder creates and adds a new recorder.
func (m *Manager) AddRecorder(cfg *types.Recorder) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.recorders[cfg.ID]; exists {
		return fmt.Errorf("recorder already exists: %s", cfg.ID)
	}

	recorder, err := NewGenericRecorder(cfg, m.ffmpegPath, m.tempDir, m.maxDurationMinutes)
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

// RemoveRecorder stops and removes a recorder.
func (m *Manager) RemoveRecorder(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	recorder, exists := m.recorders[id]
	if !exists {
		return fmt.Errorf("recorder not found: %s", id)
	}

	// Stop if recording
	if recorder.IsRecording() {
		if err := recorder.Stop(); err != nil {
			slog.Warn("error stopping recorder during removal", "id", id, "error", err)
		}
	}

	delete(m.recorders, id)
	slog.Info("recorder removed", "id", id)
	return nil
}

// UpdateRecorder updates a recorder's configuration.
func (m *Manager) UpdateRecorder(cfg *types.Recorder) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	recorder, exists := m.recorders[cfg.ID]
	if !exists {
		return fmt.Errorf("recorder not found: %s", cfg.ID)
	}

	return recorder.UpdateConfig(cfg)
}

// StartRecorder starts a specific recorder via API.
// Only on-demand recorders can be started via API. Returns error for hourly recorders.
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

// StopRecorder stops a specific recorder via API.
// Only on-demand recorders can be stopped via API. Returns error for hourly recorders.
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

// Start marks the manager as running and starts auto-start recorders.
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

	slog.Info("recording manager started", "recorders", len(m.recorders))
	return nil
}

// Stop stops all recorders and marks manager as not running.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.running = false

	// Stop cleanup scheduler
	close(m.cleanupStopCh)
	m.cleanupStopCh = make(chan struct{}) // Reset for potential restart

	var lastErr error
	for id, recorder := range m.recorders {
		if recorder.IsRecording() {
			if err := recorder.Stop(); err != nil {
				slog.Warn("error stopping recorder", "id", id, "error", err)
				lastErr = err
			}
		}
	}

	slog.Info("recording manager stopped")
	return lastErr
}

// WriteAudio sends PCM data to all active recorders.
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

// AllStatuses returns status for all recorders.
func (m *Manager) AllStatuses() map[string]types.RecorderStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]types.RecorderStatus, len(m.recorders))
	for id, recorder := range m.recorders {
		statuses[id] = recorder.Status()
	}
	return statuses
}

// APIKey returns the configured API key.
func (m *Manager) APIKey() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.apiKey
}

// SetAPIKey updates the API key.
func (m *Manager) SetAPIKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiKey = key
}
