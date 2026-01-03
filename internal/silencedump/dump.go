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

	// Output directory.
	outputDir = "/tmp/encoder-silence-dumps"
)

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

	c.silenceStartPos = c.totalWritten
	c.silenceStart = time.Now()
	c.silenceEndPos = 0
	c.capturing = true

	slog.Debug("silence dump capture started", "position", c.silenceStartPos)
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

// extractAndEncode extracts PCM from the ring buffer and encodes to MP3.
// Must be called with lock held.
func (c *Capturer) extractAndEncode() {
	// Calculate byte positions
	beforeStart := c.silenceStartPos - int64(beforeSeconds*bytesPerSecond)
	if beforeStart < 0 {
		beforeStart = 0
	}

	// Cap silence duration at maxSilenceSeconds
	actualSilenceBytes := c.silenceEndPos - c.silenceStartPos
	maxSilenceBytes := int64(maxSilenceSeconds * bytesPerSecond)
	if actualSilenceBytes > maxSilenceBytes {
		// Truncate silence: keep first half and last half of max
		actualSilenceBytes = maxSilenceBytes
	}

	afterEnd := c.silenceEndPos + int64(afterSeconds*bytesPerSecond)

	// Extract PCM data from ring buffer
	pcm := c.extractFromRing(beforeStart, c.silenceStartPos, c.silenceEndPos, actualSilenceBytes, afterEnd)

	silenceStart := c.silenceStart
	silenceDuration := time.Duration(c.silenceEndPos-c.silenceStartPos) * time.Second / time.Duration(bytesPerSecond)

	// Encode in background to not block audio processing
	go func() {
		result := c.encodeToMP3(pcm, silenceStart, silenceDuration)
		if c.onDumpReady != nil {
			c.onDumpReady(result)
		}
	}()
}

// extractFromRing extracts audio data from the ring buffer.
// Handles wrap-around and silence truncation.
func (c *Capturer) extractFromRing(beforeStart, silenceStart, silenceEnd, maxSilenceBytes, afterEnd int64) []byte {
	// Calculate total output size
	beforeBytes := silenceStart - beforeStart
	afterBytes := afterEnd - silenceEnd

	// Use actual silence bytes (may be truncated)
	actualSilenceBytes := silenceEnd - silenceStart
	if actualSilenceBytes > maxSilenceBytes {
		actualSilenceBytes = maxSilenceBytes
	}

	totalBytes := beforeBytes + actualSilenceBytes + afterBytes
	result := make([]byte, totalBytes)

	// Copy "before" section
	c.copyFromRing(result[:beforeBytes], beforeStart)

	// Copy silence section (possibly truncated)
	if actualSilenceBytes > 0 {
		c.copyFromRing(result[beforeBytes:beforeBytes+actualSilenceBytes], silenceStart)
	}

	// Copy "after" section
	c.copyFromRing(result[beforeBytes+actualSilenceBytes:], silenceEnd)

	return result
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
func (c *Capturer) encodeToMP3(pcm []byte, silenceStart time.Time, duration time.Duration) *EncodeResult {
	result := &EncodeResult{
		Duration:  duration,
		DumpStart: silenceStart,
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		result.Error = fmt.Errorf("create output dir: %w", err)
		return result
	}

	// Generate filename: 2024-01-15_14-32-05.mp3 (local time)
	result.Filename = silenceStart.Local().Format("2006-01-02_15-04-05") + ".mp3"
	result.FilePath = filepath.Join(outputDir, result.Filename)

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

	cmd := exec.CommandContext(ctx, c.ffmpegPath, args...)
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

	// Zero out the buffer
	for i := range c.buffer {
		c.buffer[i] = 0
	}

	slog.Debug("silence dump capturer reset")
}
