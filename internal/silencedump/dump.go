// Package silencedump captures audio around silence and channel-imbalance
// incidents and encodes the context to MP3.
package silencedump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const (
	// Dump timing.
	beforeSeconds      = 15
	maxIncidentSeconds = 5
	afterSeconds       = 15
	bufferSeconds      = beforeSeconds + maxIncidentSeconds + afterSeconds // 35 seconds

	// Buffer capacity in bytes.
	bufferCapacity = bufferSeconds * audio.BytesPerSecond // ~6.7 MB

	// MP3 encoding settings.
	mp3Bitrate    = "64k"
	encodeTimeout = 30 * time.Second

	// Output subdirectory name prefix (inside system temp dir).
	outputDirPrefix = "encoder-silence-dumps"
)

// Trigger identifies the audio incident that requested a dump.
type Trigger string

const (
	// TriggerSilence identifies a dump captured around a silence incident.
	TriggerSilence Trigger = "silence"
	// TriggerChannelImbalance identifies a dump captured around an L/R imbalance incident.
	TriggerChannelImbalance Trigger = "channel_imbalance"
)

// outputDirForPort returns the legacy output directory for audio incident dumps, unique per port.
func outputDirForPort(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", outputDirPrefix, port))
}

// EncodeResult contains the result of encoding an audio incident dump.
type EncodeResult struct {
	Trigger    Trigger
	IncidentID audio.IncidentID
	FilePath   string
	Filename   string
	FileSize   int64
	Duration   time.Duration
	DumpStart  time.Time
	Error      error
}

type captureKey struct {
	trigger    Trigger
	incidentID audio.IncidentID
}

type captureState struct {
	startPos    int64
	endPos      int64
	startedAt   time.Time
	recovered   bool
	savedBefore []byte
}

// DumpCallback is called when a dump is ready.
type DumpCallback func(result *EncodeResult)

// Capturer captures audio context around silence and channel-imbalance incidents.
type Capturer struct {
	mu sync.Mutex

	// Ring buffer for continuous audio capture.
	buffer       []byte
	writePos     int   // current write position in buffer
	totalWritten int64 // total bytes written (for position tracking)

	// Incident captures share the continuous ring buffer. The detector identity
	// allows a recovered capture and a new capture of the same trigger to coexist.
	captures map[captureKey]*captureState

	// Configuration.
	ffmpegPath  string
	outputDir   string
	enabled     bool
	onDumpReady DumpCallback
}

// NewCapturer creates a new audio incident dump capturer. The ~6.7 MB ring buffer is
// allocated lazily on the first enabled WriteAudio so installs that never
// enable incident dumps do not pay for it.
func NewCapturer(ffmpegPath, outputDir string, onDumpReady DumpCallback) *Capturer {
	return &Capturer{
		ffmpegPath:  ffmpegPath,
		outputDir:   outputDir,
		enabled:     ffmpegPath != "",
		onDumpReady: onDumpReady,
	}
}

// SetEnabled sets whether dump capture is active. Disabling releases the ring
// buffer and clears any capture in progress.
func (c *Capturer) SetEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled && c.ffmpegPath != ""
	if !c.enabled {
		c.buffer = nil
		c.resetLocked()
	}
	c.mu.Unlock()
}

// Enabled reports whether dump capture is active.
func (c *Capturer) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// WriteAudio buffers incoming PCM data for potential audio-incident dumps.
func (c *Capturer) WriteAudio(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || len(pcm) == 0 {
		return
	}

	if c.buffer == nil {
		c.buffer = make([]byte, bufferCapacity)
	}

	// Write to ring buffer with wrap-around
	c.writePos = c.writeToRing(pcm)
	c.totalWritten += int64(len(pcm))

	// Check if we have enough recovery audio to finalize
	c.checkAndFinalize()
}

// OnIncidentStart begins capturing audio context for a potential incident dump.
func (c *Capturer) OnIncidentStart(trigger Trigger, incidentID audio.IncidentID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled {
		return
	}

	// The captures map is created lazily here, its only write site.
	if c.captures == nil {
		c.captures = map[captureKey]*captureState{}
	}

	// Snapshot pre-incident audio to prevent loss when an incident outlives the ring.
	beforeBytes := min(c.totalWritten, int64(beforeSeconds*audio.BytesPerSecond))
	state := &captureState{
		startPos:  c.totalWritten,
		startedAt: time.Now(),
	}
	if beforeBytes > 0 {
		state.savedBefore = make([]byte, beforeBytes)
		c.copyFromRing(state.savedBefore, c.totalWritten-beforeBytes)
	}
	key := captureKey{trigger: trigger, incidentID: incidentID}
	c.captures[key] = state

	slog.Debug("audio dump capture started",
		"trigger", trigger,
		"incident_id", incidentID,
		"position", state.startPos,
		"saved_before_bytes", len(state.savedBefore),
	)
}

// OnIncidentRecover signals that audio has recovered from an incident.
// recoveryDuration is how long audio was good before recovery was confirmed;
// the incident end position is backdated by this amount to capture when audio
// actually returned.
func (c *Capturer) OnIncidentRecover(
	trigger Trigger, incidentID audio.IncidentID, totalDuration, recoveryDuration time.Duration,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.captures[captureKey{trigger: trigger, incidentID: incidentID}]
	if !c.enabled || state == nil {
		return
	}

	// Backdate silenceEndPos to when audio actually returned, not when recovery was confirmed.
	// The JustRecovered event fires after recoveryDuration has elapsed, so we need to
	// subtract that amount to capture the moment audio came back.
	//
	// Wall-clock recovery can outpace bytes written; clamp so copyFromRing never
	// receives a start before the incident start position.
	recoveryBytes := int64(recoveryDuration.Seconds() * float64(audio.BytesPerSecond))
	state.endPos = max(state.startPos, c.totalWritten-recoveryBytes)
	state.recovered = true

	slog.Debug("audio dump recovery detected",
		"trigger", trigger,
		"incident_id", incidentID,
		"start_pos", state.startPos,
		"end_pos", state.endPos,
		"duration", totalDuration,
		"recovery_duration", recoveryDuration,
	)
}

// checkAndFinalize completes a dump capture if sufficient audio context is available.
func (c *Capturer) checkAndFinalize() {
	for key, state := range c.captures {
		if !state.recovered {
			continue
		}
		requiredBytes := state.endPos + int64(afterSeconds*audio.BytesPerSecond)
		if c.totalWritten < requiredBytes {
			continue
		}
		c.extractAndEncode(key, state)
		delete(c.captures, key)
	}
}

// extractAndEncode encodes buffered audio to an MP3 file.
func (c *Capturer) extractAndEncode(key captureKey, state *captureState) {
	// Calculate section sizes (the incident itself is capped to bound memory and output size).
	incidentBytes := min(max(0, state.endPos-state.startPos), int64(maxIncidentSeconds*audio.BytesPerSecond))
	afterBytes := int64(0)
	if state.recovered {
		afterBytes = int64(afterSeconds * audio.BytesPerSecond)
	}

	// Build PCM: savedBefore (guaranteed intact) + incident (capped) + after.
	beforeLen := int64(len(state.savedBefore))
	pcm := make([]byte, beforeLen+incidentBytes+afterBytes)
	copy(pcm, state.savedBefore)
	c.copyFromRing(pcm[beforeLen:beforeLen+incidentBytes], state.startPos)
	c.copyFromRing(pcm[beforeLen+incidentBytes:], state.endPos)

	// Capture all values needed for encoding before releasing lock
	incidentStart := state.startedAt
	incidentDuration := time.Duration(state.endPos-state.startPos) * time.Second / time.Duration(audio.BytesPerSecond)
	ffmpegPath := c.ffmpegPath
	outputDir := c.outputDir
	callback := c.onDumpReady

	// Clear savedBefore to free memory (no longer needed after extraction).
	state.savedBefore = nil

	// Encode in background to not block audio processing.
	// All values are captured above; goroutine doesn't access Capturer fields.
	go func() {
		result := encodeToMP3(ffmpegPath, outputDir, pcm, key.trigger, key.incidentID, incidentStart, incidentDuration)
		if callback != nil {
			callback(result)
		}
	}()
}

// writeToRing copies src into the ring and returns the next write position.
//
// Precondition: len(src) <= bufferCapacity. Distributor chunks are smaller than
// the ring, so one write wraps at most once.
func (c *Capturer) writeToRing(src []byte) int {
	n := copy(c.buffer[c.writePos:], src)
	if n < len(src) {
		return copy(c.buffer, src[n:]) // wrapped: remainder goes to the front
	}
	pos := c.writePos + n
	if pos == bufferCapacity {
		return 0 // landed exactly on the end; next write resumes at the front
	}
	return pos
}

// copyFromRing copies len(dst) bytes from startPos using at most two copy calls.
//
// Precondition: startPos >= 0 and len(dst) <= bufferCapacity. Silence snapshots
// are smaller than the ring, so one read wraps at most once.
func (c *Capturer) copyFromRing(dst []byte, startPos int64) {
	pos := int(startPos % int64(bufferCapacity))
	n := copy(dst, c.buffer[pos:])
	copy(dst[n:], c.buffer) // continuation after a single wrap; no-op when the read fit before the end
}

func dumpFilename(trigger Trigger, incidentStart time.Time) string {
	filename := incidentStart.Local().Format("2006-01-02_15-04-05") + ".mp3"
	if trigger == TriggerChannelImbalance {
		return "channel-imbalance-" + filename
	}
	return filename
}

// encodeToMP3 encodes PCM audio to an MP3 file.
func encodeToMP3(
	ffmpegPath, outputDir string, pcm []byte,
	trigger Trigger, incidentID audio.IncidentID, incidentStart time.Time, duration time.Duration,
) *EncodeResult {
	result := &EncodeResult{
		Trigger:    trigger,
		IncidentID: incidentID,
		Duration:   duration,
		DumpStart:  incidentStart,
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // Dump directory needs to be readable
		result.Error = fmt.Errorf("create output dir: %w", err)
		return result
	}

	// Generate filename: 2024-01-15_14-32-05.mp3 (local time)
	result.Filename = dumpFilename(trigger, incidentStart)
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

	cmd := util.CommandContext(ctx, ffmpegPath, args...)
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

	slog.Info("audio dump encoded",
		"trigger", trigger,
		"incident_id", incidentID,
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

	c.resetLocked()

	slog.Debug("audio incident dump capturer reset")
}

// resetLocked clears all capture state. The caller must hold c.mu.
func (c *Capturer) resetLocked() {
	c.writePos = 0
	c.totalWritten = 0
	clear(c.captures)
}
