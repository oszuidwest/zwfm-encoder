// Package silencedump captures audio around silence events and encodes to MP3.
package silencedump

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// Audio format constants (must match encoder's PCM format).
	bytesPerSecond = types.SampleRate * types.Channels * 2 // 48kHz * 2ch * 16bit = 192000

	// Dump timing.
	beforeSeconds     = 15
	maxSilenceSeconds = 5
	afterSeconds      = 15
	bufferSeconds     = beforeSeconds + maxSilenceSeconds + afterSeconds // 35 seconds

	// Buffer capacity in bytes.
	bufferCapacity = bufferSeconds * bytesPerSecond // ~6.7 MB

	// MP3 encoding settings.
	mp3Bitrate    = "64k"
	encodeTimeout = 30 * time.Second

	// Output subdirectory name (inside system temp dir).
	outputDirName = "encoder-silence-dumps"
)

// outputDir returns the platform-appropriate output directory for silence dumps.
func outputDir() string {
	return filepath.Join(os.TempDir(), outputDirName)
}

// EncodeResult contains the result of encoding a silence dump.
type EncodeResult struct {
	FilePath  string        // Full path to the MP3 file
	Filename  string        // Just the filename
	FileSize  int64         // Size in bytes
	Duration  time.Duration // Total silence duration
	DumpStart time.Time     // When silence started
	Error     error         // nil if successful
}

// DumpCallback is called when a dump is ready.
type DumpCallback func(result *EncodeResult)

// Capturer captures audio around silence events using a ring buffer.
type Capturer struct {
	mu sync.Mutex

	// Ring buffer for continuous audio capture.
	buffer       []byte
	writePos     int   // Current write position in buffer
	totalWritten int64 // Total bytes written (for position tracking)

	// Silence event tracking (positions, not copies).
	silenceStartPos int64     // Byte position when silence started
	silenceEndPos   int64     // Byte position when recovery started
	silenceStart    time.Time // Time when silence started
	capturing       bool      // True if we're waiting for recovery + 15s

	// Saved pre-silence audio snapshot. Captured immediately on silence start
	// to prevent data loss during long silences that exceed ring buffer capacity.
	savedBefore []byte

	// Configuration.
	ffmpegPath  string
	enabled     bool
	onDumpReady DumpCallback
}

// NewCapturer creates a new silence dump capturer.
func NewCapturer(ffmpegPath string, onDumpReady DumpCallback) *Capturer {
	return &Capturer{
		buffer:      make([]byte, bufferCapacity),
		ffmpegPath:  ffmpegPath,
		enabled:     ffmpegPath != "",
		onDumpReady: onDumpReady,
	}
}

// SetEnabled controls whether dump capture is active.
func (c *Capturer) SetEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled && c.ffmpegPath != ""
	c.mu.Unlock()
}

// WriteAudio writes PCM data to the ring buffer.
// Called for every audio chunk while encoder is running.
func (c *Capturer) WriteAudio(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || len(pcm) == 0 {
		return
	}

	// Write to ring buffer with wrap-around
	for i := 0; i < len(pcm); i++ {
		c.buffer[c.writePos] = pcm[i]
		c.writePos = (c.writePos + 1) % bufferCapacity
	}
	c.totalWritten += int64(len(pcm))

	// Check if we have enough recovery audio to finalize
	c.checkAndFinalize()
}

// OnSilenceStart marks the position when silence is detected.
func (c *Capturer) OnSilenceStart() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled {
		return
	}

	// If already capturing with recovery detected, finalize current dump first.
	// This prevents losing silence1 when silence2 starts before 15s post-recovery completes.
	if c.capturing && c.silenceEndPos > 0 {
		c.extractAndEncode()
	}

	// Save "before" audio immediately to prevent loss during long silences.
	// This snapshot is taken now, guaranteeing we have the pre-silence context
	// even if silence lasts longer than the ring buffer can hold.
	beforeBytes := int64(beforeSeconds * bytesPerSecond)
	if c.totalWritten < beforeBytes {
		beforeBytes = c.totalWritten
	}
	if beforeBytes > 0 {
		c.savedBefore = make([]byte, beforeBytes)
		c.copyFromRing(c.savedBefore, c.totalWritten-beforeBytes)
	} else {
		c.savedBefore = nil
	}

	c.silenceStartPos = c.totalWritten
	c.silenceStart = time.Now()
	c.silenceEndPos = 0
	c.capturing = true

	slog.Debug("silence dump capture started", "position", c.silenceStartPos, "saved_before_bytes", len(c.savedBefore))
}

// OnSilenceRecover marks the position when audio recovers.
func (c *Capturer) OnSilenceRecover(totalDuration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || !c.capturing {
		return
	}

	c.silenceEndPos = c.totalWritten

	slog.Debug("silence dump recovery detected",
		"start_pos", c.silenceStartPos,
		"end_pos", c.silenceEndPos,
		"duration", totalDuration,
	)
}

// checkAndFinalize checks if enough recovery audio has been captured.
// Must be called with lock held.
func (c *Capturer) checkAndFinalize() {
	if !c.capturing || c.silenceEndPos == 0 {
		return
	}

	// Wait for 15 seconds of audio after recovery
	requiredBytes := c.silenceEndPos + int64(afterSeconds*bytesPerSecond)
	if c.totalWritten < requiredBytes {
		return
	}

	// Extract and encode the dump
	c.extractAndEncode()

	// Reset state for next silence event
	c.capturing = false
	c.silenceStartPos = 0
	c.silenceEndPos = 0
	c.silenceStart = time.Time{}
}

// extractAndEncode extracts PCM from savedBefore + ring buffer and encodes to MP3.
// Must be called with lock held. The lock is held during buffer extraction
// to ensure data consistency (prevent WriteAudio from overwriting data we need).
func (c *Capturer) extractAndEncode() {
	// Cap silence duration at maxSilenceSeconds
	silenceBytes := c.silenceEndPos - c.silenceStartPos
	if silenceBytes < 0 {
		silenceBytes = 0 // No recovery yet (early finalization during ongoing silence)
	}
	maxSilenceBytes := int64(maxSilenceSeconds * bytesPerSecond)
	if silenceBytes > maxSilenceBytes {
		silenceBytes = maxSilenceBytes
	}

	// Calculate "after" bytes (0 if no recovery yet)
	afterBytes := int64(0)
	if c.silenceEndPos > 0 {
		afterBytes = int64(afterSeconds * bytesPerSecond)
	}

	// Build PCM: savedBefore (guaranteed intact) + silence (capped) + after
	beforeLen := int64(len(c.savedBefore))
	totalBytes := beforeLen + silenceBytes + afterBytes
	pcm := make([]byte, totalBytes)

	// Copy "before" from saved snapshot (never lost, even for long silences)
	copy(pcm, c.savedBefore)

	// Copy silence section from ring buffer (capped at maxSilenceSeconds)
	if silenceBytes > 0 {
		c.copyFromRing(pcm[beforeLen:beforeLen+silenceBytes], c.silenceStartPos)
	}

	// Copy "after" section from ring buffer
	if afterBytes > 0 {
		c.copyFromRing(pcm[beforeLen+silenceBytes:], c.silenceEndPos)
	}

	// Capture all values needed for encoding before releasing lock
	silenceStart := c.silenceStart
	silenceDuration := time.Duration(c.silenceEndPos-c.silenceStartPos) * time.Second / time.Duration(bytesPerSecond)
	ffmpegPath := c.ffmpegPath
	callback := c.onDumpReady

	// Clear savedBefore to free memory (no longer needed after extraction)
	c.savedBefore = nil

	// Encode in background to not block audio processing.
	// All values are captured above; goroutine doesn't access Capturer fields.
	go func() {
		result := encodeToMP3(ffmpegPath, pcm, silenceStart, silenceDuration)
		if callback != nil {
			callback(result)
		}
	}()
}

// copyFromRing copies data from the ring buffer starting at the given position.
func (c *Capturer) copyFromRing(dst []byte, startPos int64) {
	// Calculate start position in buffer, accounting for wrap-around
	bufferStart := startPos % int64(bufferCapacity)

	for i := range dst {
		pos := (bufferStart + int64(i)) % int64(bufferCapacity)
		dst[i] = c.buffer[pos]
	}
}

// encodeToMP3 encodes PCM audio to an MP3 file.
func encodeToMP3(ffmpegPath string, pcm []byte, silenceStart time.Time, duration time.Duration) *EncodeResult {
	result := &EncodeResult{
		Duration:  duration,
		DumpStart: silenceStart,
	}

	// Ensure output directory exists
	outDir := outputDir()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		result.Error = fmt.Errorf("create output dir: %w", err)
		return result
	}

	// Generate filename: 2024-01-15_14-32-05.mp3 (local time)
	result.Filename = silenceStart.Local().Format("2006-01-02_15-04-05") + ".mp3"
	result.FilePath = filepath.Join(outDir, result.Filename)

	// Build FFmpeg command
	ctx, cancel := context.WithTimeout(context.Background(), encodeTimeout)
	defer cancel()

	args := []string{
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", types.SampleRate),
		"-ac", fmt.Sprintf("%d", types.Channels),
		"-i", "pipe:0",
		"-c:a", "libmp3lame",
		"-b:a", mp3Bitrate,
		"-f", "mp3",
		"-y", // Overwrite if exists
		result.FilePath,
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(pcm)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		result.Error = fmt.Errorf("ffmpeg encoding failed: %w, stderr: %s", err, stderr.String())
		return result
	}

	// Get file size
	info, err := os.Stat(result.FilePath)
	if err != nil {
		result.Error = fmt.Errorf("stat output file: %w", err)
		return result
	}
	result.FileSize = info.Size()

	slog.Info("silence dump encoded",
		"file", result.Filename,
		"size", result.FileSize,
		"duration", duration,
	)

	return result
}

// Reset clears all capture state.
func (c *Capturer) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writePos = 0
	c.totalWritten = 0
	c.silenceStartPos = 0
	c.silenceEndPos = 0
	c.silenceStart = time.Time{}
	c.capturing = false
	c.savedBefore = nil // Free memory

	slog.Debug("silence dump capturer reset")
}
