// Package silencedump captures audio around silence events and encodes to MP3.
package silencedump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
)

const (
	// Audio format constants (must match encoder's PCM format).
	bytesPerSecond = audio.SampleRate * audio.Channels * 2 // 48kHz * 2ch * 16bit = 192000

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

	// Output subdirectory name prefix (inside system temp dir).
	outputDirPrefix = "encoder-silence-dumps"
)

// outputDirForPort returns the output directory for silence dumps, unique per port.
func outputDirForPort(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", outputDirPrefix, port))
}

// EncodeResult contains the result of encoding a silence dump.
type EncodeResult struct {
	// FilePath is the full path to the MP3 file.
	FilePath string
	// Filename is the base name of the MP3 file.
	Filename string
	// FileSize is the MP3 size in bytes.
	FileSize int64
	// Duration is the total silence event duration.
	Duration time.Duration
	// DumpStart is when silence started.
	DumpStart time.Time
	// Error is non-nil if encoding failed.
	Error error
}

// DumpCallback is called when a dump is ready.
type DumpCallback func(result *EncodeResult)

// Capturer captures audio context around silence events for debugging.
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
	// capturing reports whether we're waiting for recovery + 15s.
	capturing bool

	// Saved pre-silence audio snapshot. Captured immediately on silence start
	// to prevent data loss during long silences that exceed ring buffer capacity.
	savedBefore []byte

	// Configuration.
	ffmpegPath  string
	outputDir   string
	enabled     bool
	onDumpReady DumpCallback
}

// NewCapturer creates a new silence dump capturer.
func NewCapturer(ffmpegPath, outputDir string, onDumpReady DumpCallback) *Capturer {
	return &Capturer{
		buffer:      make([]byte, bufferCapacity),
		ffmpegPath:  ffmpegPath,
		outputDir:   outputDir,
		enabled:     ffmpegPath != "",
		onDumpReady: onDumpReady,
	}
}

// SetEnabled sets whether dump capture is active.
func (c *Capturer) SetEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled && c.ffmpegPath != ""
	c.mu.Unlock()
}

// WriteAudio buffers incoming PCM data for potential silence dump capture.
func (c *Capturer) WriteAudio(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || len(pcm) == 0 {
		return
	}

	// Write to ring buffer with wrap-around
	for i := range pcm {
		c.buffer[c.writePos] = pcm[i]
		c.writePos = (c.writePos + 1) % bufferCapacity
	}
	c.totalWritten += int64(len(pcm))

	// Check if we have enough recovery audio to finalize
	c.checkAndFinalize()
}

// OnSilenceStart begins capturing audio context for a potential silence dump.
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

	// Snapshot pre-silence audio to prevent loss during long silences
	beforeBytes := min(c.totalWritten, int64(beforeSeconds*bytesPerSecond))
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

// OnSilenceRecover signals that audio has recovered from silence.
// recoveryDuration is how long audio was good before recovery was confirmed.
// We backdate silenceEndPos by this amount to capture when audio actually returned.
func (c *Capturer) OnSilenceRecover(totalDuration, recoveryDuration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || !c.capturing {
		return
	}

	// Backdate silenceEndPos to when audio actually returned, not when recovery was confirmed.
	// The JustRecovered event fires after recoveryDuration has elapsed, so we need to
	// subtract that amount to capture the moment audio came back.
	recoveryBytes := int64(recoveryDuration.Seconds() * float64(bytesPerSecond))
	c.silenceEndPos = c.totalWritten - recoveryBytes

	slog.Debug("silence dump recovery detected",
		"start_pos", c.silenceStartPos,
		"end_pos", c.silenceEndPos,
		"duration", totalDuration,
		"recovery_duration", recoveryDuration,
	)
}

// checkAndFinalize completes a dump capture if sufficient audio context is available.
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

// extractAndEncode encodes buffered audio to an MP3 file.
func (c *Capturer) extractAndEncode() {
	// Calculate section sizes (silence capped at maxSilenceSeconds)
	silenceBytes := min(max(0, c.silenceEndPos-c.silenceStartPos), int64(maxSilenceSeconds*bytesPerSecond))
	afterBytes := int64(0)
	if c.silenceEndPos > 0 {
		afterBytes = int64(afterSeconds * bytesPerSecond)
	}

	// Build PCM: savedBefore (guaranteed intact) + silence (capped) + after
	beforeLen := int64(len(c.savedBefore))
	pcm := make([]byte, beforeLen+silenceBytes+afterBytes)
	copy(pcm, c.savedBefore)
	c.copyFromRing(pcm[beforeLen:beforeLen+silenceBytes], c.silenceStartPos)
	c.copyFromRing(pcm[beforeLen+silenceBytes:], c.silenceEndPos)

	// Capture all values needed for encoding before releasing lock
	silenceStart := c.silenceStart
	silenceDuration := time.Duration(c.silenceEndPos-c.silenceStartPos) * time.Second / time.Duration(bytesPerSecond)
	ffmpegPath := c.ffmpegPath
	outputDir := c.outputDir
	callback := c.onDumpReady

	// Clear savedBefore to free memory (no longer needed after extraction)
	c.savedBefore = nil

	// Encode in background to not block audio processing.
	// All values are captured above; goroutine doesn't access Capturer fields.
	go func() {
		result := encodeToMP3(ffmpegPath, outputDir, pcm, silenceStart, silenceDuration)
		if callback != nil {
			callback(result)
		}
	}()
}

// copyFromRing copies buffered audio data into the destination slice.
func (c *Capturer) copyFromRing(dst []byte, startPos int64) {
	// Calculate start position in buffer, accounting for wrap-around
	bufferStart := startPos % int64(bufferCapacity)

	for i := range dst {
		pos := (bufferStart + int64(i)) % int64(bufferCapacity)
		dst[i] = c.buffer[pos]
	}
}

// encodeToMP3 encodes PCM audio to an MP3 file.
func encodeToMP3(ffmpegPath, outputDir string, pcm []byte, silenceStart time.Time, duration time.Duration) *EncodeResult {
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
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		encodeTimeout,
		errors.New("ffmpeg encode timeout"),
	)
	defer cancel()

	args := ffmpeg.BaseInputArgs()
	args = append(args,
		"-c:a", "libmp3lame",
		"-b:a", mp3Bitrate,
		"-f", "mp3",
		"-y", // Overwrite if exists
		result.FilePath,
	)

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
