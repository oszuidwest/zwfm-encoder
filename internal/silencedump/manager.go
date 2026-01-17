package silencedump

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Manager coordinates silence dump capture and file retention.
type Manager struct {
	mu sync.RWMutex

	capturer    *Capturer
	ffmpegPath  string
	outputDir   string
	enabled     bool
	onDumpReady DumpCallback

	// Cleanup configuration
	retentionDays int

	// Cleanup scheduler
	cleanupStopCh chan struct{}
	running       bool
}

// NewManager creates a new silence dump manager.
func NewManager(ffmpegPath string, port int, enabled bool, retentionDays int, onDumpReady DumpCallback) *Manager {
	outputDir := outputDirForPort(port)

	m := &Manager{
		ffmpegPath:    ffmpegPath,
		outputDir:     outputDir,
		enabled:       enabled && ffmpegPath != "",
		onDumpReady:   onDumpReady,
		retentionDays: retentionDays,
	}

	// Create capturer if FFmpeg is available
	if ffmpegPath != "" {
		m.capturer = NewCapturer(ffmpegPath, outputDir, onDumpReady)
		m.capturer.SetEnabled(m.enabled)
	}

	return m
}

// Start begins automatic cleanup of old dump files.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return
	}

	m.running = true
	m.cleanupStopCh = make(chan struct{})
	m.startCleanupScheduler()

	slog.Info("silence dump manager started", "enabled", m.enabled)
}

// Stop stops automatic cleanup and releases resources.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false

	if m.cleanupStopCh != nil {
		close(m.cleanupStopCh)
		m.cleanupStopCh = nil
	}
	m.mu.Unlock()

	// Reset capturer outside lock
	if m.capturer != nil {
		m.capturer.Reset()
	}

	slog.Info("silence dump manager stopped")
}

// WriteAudio buffers PCM audio data for potential dump capture.
func (m *Manager) WriteAudio(pcm []byte) {
	if m.capturer != nil {
		m.capturer.WriteAudio(pcm)
	}
}

// HandleSilenceEvent processes silence detection events.
func (m *Manager) HandleSilenceEvent(event audio.SilenceEvent) {
	if m.capturer == nil {
		return
	}

	if event.JustEntered {
		m.capturer.OnSilenceStart()
	}

	if event.JustRecovered {
		m.capturer.OnSilenceRecover(
			time.Duration(event.TotalDurationMs)*time.Millisecond,
			time.Duration(event.RecoveryDurationMs)*time.Millisecond,
		)
	}
}

// SetEnabled controls whether dump capture is active.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	m.enabled = enabled && m.ffmpegPath != ""
	m.mu.Unlock()

	if m.capturer != nil {
		m.capturer.SetEnabled(enabled)
	}
}

// SetRetentionDays sets the cleanup retention period.
func (m *Manager) SetRetentionDays(days int) {
	m.mu.Lock()
	m.retentionDays = days
	m.mu.Unlock()
}

// startCleanupScheduler schedules hourly cleanup of old dump files.
func (m *Manager) startCleanupScheduler() {
	go func() {
		for {
			duration := util.TimeUntilNextHour(time.Now())

			slog.Debug("silence dump cleanup: next run scheduled", "in", duration.Round(time.Second))

			select {
			case <-time.After(duration):
				m.runCleanup()
			case <-m.cleanupStopCh:
				slog.Debug("silence dump cleanup scheduler stopped")
				return
			}
		}
	}()
}

// runCleanup removes dump files older than retention days.
func (m *Manager) runCleanup() {
	m.mu.RLock()
	retentionDays := m.retentionDays
	m.mu.RUnlock()

	// Skip if retention is 0 (keep forever)
	if retentionDays == 0 {
		return
	}

	m.mu.RLock()
	dir := m.outputDir
	m.mu.RUnlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Directory might not exist yet, which is fine
		if !os.IsNotExist(err) {
			slog.Warn("silence dump cleanup: failed to read directory", "path", dir, "error", err)
		}
		return
	}

	cutoff := util.OldestToKeep(retentionDays, time.Now())
	var deleted int

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Only process .mp3 files
		if filepath.Ext(name) != ".mp3" {
			continue
		}

		// Extract date from filename
		fileDate, ok := util.FilenameTime(name)
		if !ok {
			continue
		}

		// Delete if older than retention
		if fileDate.Before(cutoff) {
			filePath := filepath.Join(dir, name)
			if err := os.Remove(filePath); err != nil {
				slog.Warn("silence dump cleanup: failed to delete file", "path", filePath, "error", err)
			} else {
				deleted++
				slog.Debug("silence dump cleanup: deleted file", "file", name)
			}
		}
	}

	if deleted > 0 {
		slog.Info("silence dump cleanup: deleted old files", "count", deleted)
	}
}
