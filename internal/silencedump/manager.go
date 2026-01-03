package silencedump

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// datePattern matches date in filename: YYYY-MM-DD_HH-MM-SS.mp3
var datePattern = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)

// Manager orchestrates silence dump capture and cleanup.
type Manager struct {
	mu sync.RWMutex

	capturer    *Capturer
	ffmpegPath  string
	enabled     bool
	onDumpReady DumpCallback

	// Cleanup configuration
	retentionDays int

	// Cleanup scheduler
	cleanupStopCh chan struct{}
	running       bool
}

// NewManager creates a new silence dump manager.
func NewManager(ffmpegPath string, onDumpReady DumpCallback) *Manager {
	m := &Manager{
		ffmpegPath:    ffmpegPath,
		enabled:       ffmpegPath != "",
		onDumpReady:   onDumpReady,
		retentionDays: 7, // Default: 7 days
	}

	// Create capturer if FFmpeg is available
	if m.enabled {
		m.capturer = NewCapturer(ffmpegPath, onDumpReady)
	}

	return m
}

// Start begins the cleanup scheduler.
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

// Stop stops the cleanup scheduler and resets the capturer.
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

// WriteAudio feeds PCM audio data to the capturer.
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
		m.capturer.OnSilenceRecover(time.Duration(event.TotalDurationMs) * time.Millisecond)
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

// startCleanupScheduler starts the daily cleanup scheduler.
func (m *Manager) startCleanupScheduler() {
	go func() {
		for {
			// Calculate duration until next 03:00
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
			if now.After(next) {
				next = next.Add(24 * time.Hour)
			}
			duration := next.Sub(now)

			slog.Debug("silence dump cleanup: next run scheduled", "at", next.Format(time.DateTime))

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

	dir := outputDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Directory might not exist yet, which is fine
		if !os.IsNotExist(err) {
			slog.Warn("silence dump cleanup: failed to read directory", "path", dir, "error", err)
		}
		return
	}

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
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
		fileDate, ok := extractDateFromFilename(name)
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

// extractDateFromFilename extracts the date from a filename like "2025-01-15_14-32-05.mp3".
func extractDateFromFilename(filename string) (time.Time, bool) {
	matches := datePattern.FindStringSubmatch(filename)
	if len(matches) < 2 {
		return time.Time{}, false
	}

	date, err := time.Parse("2006-01-02", matches[1])
	if err != nil {
		return time.Time{}, false
	}

	return date, true
}
