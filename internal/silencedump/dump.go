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

	// maxRecoveredCaptures bounds post-recovery snapshots per trigger.
	// The oldest snapshot is finalized with available audio when the limit is reached.
	maxRecoveredCaptures = 2
)

// Trigger identifies the audio incident that requested a dump.
type Trigger string

const (
	// TriggerSilence identifies a dump captured around a silence incident.
	TriggerSilence Trigger = "silence"
	// TriggerChannelImbalance identifies a dump captured around an L/R imbalance incident.
	TriggerChannelImbalance Trigger = "channel_imbalance"
)

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

	// captures keeps overlapping post-recovery windows distinct by detector incident.
	captures map[captureKey]*captureState

	// Configuration.
	ffmpegPath  string
	outputDir   string
	enabled     bool
	onDumpReady DumpCallback
}

// NewCapturer creates an audio incident dump capturer.
// Its ring buffer is allocated lazily while capture is enabled.
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

	c.writePos = c.writeToRing(pcm)
	c.totalWritten += int64(len(pcm))

	c.checkAndFinalize()
}

// OnIncidentStart begins capturing audio context for a potential incident dump.
func (c *Capturer) OnIncidentStart(trigger Trigger, incidentID audio.IncidentID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled {
		return
	}

	if c.captures == nil {
		c.captures = map[captureKey]*captureState{}
	}

	// Bound per-trigger memory when detectors reset or incidents flap.
	var oldestKey captureKey
	var oldest *captureState
	recoveredCount := 0
	for k, s := range c.captures {
		if k.trigger != trigger {
			continue
		}
		if !s.recovered {
			delete(c.captures, k)
			continue
		}
		recoveredCount++
		if oldest == nil || s.startedAt.Before(oldest.startedAt) {
			oldestKey, oldest = k, s
		}
	}
	if recoveredCount >= maxRecoveredCaptures {
		c.extractAndEncode(oldestKey, oldest)
		delete(c.captures, oldestKey)
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

// OnIncidentRecover records a recovery at the time audio returned.
// recoveryDuration compensates for the detector's confirmation delay.
func (c *Capturer) OnIncidentRecover(
	trigger Trigger, incidentID audio.IncidentID, totalDuration, recoveryDuration time.Duration,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.captures[captureKey{trigger: trigger, incidentID: incidentID}]
	if !c.enabled || state == nil {
		return
	}

	// Clamp because wall-clock recovery can advance faster than PCM input.
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
// At most one capture finalizes per call: each extract copies multiple MB while
// holding the lock the audio path needs, and coinciding recoveries would otherwise
// stack those copies into a single buffer write. Remaining recovered captures
// finalize on the next writes.
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
		return
	}
}

// extractAndEncode encodes buffered audio to an MP3 file.
func (c *Capturer) extractAndEncode(key captureKey, state *captureState) {
	// Cap incident audio and prevent early finalization from reading stale ring data.
	incidentBytes := min(max(0, state.endPos-state.startPos), int64(maxIncidentSeconds*audio.BytesPerSecond))
	afterBytes := min(int64(afterSeconds*audio.BytesPerSecond), c.totalWritten-state.endPos)

	beforeLen := int64(len(state.savedBefore))
	pcm := make([]byte, beforeLen+incidentBytes+afterBytes)
	copy(pcm, state.savedBefore)
	c.copyFromRing(pcm[beforeLen:beforeLen+incidentBytes], state.startPos)
	c.copyFromRing(pcm[beforeLen+incidentBytes:], state.endPos)

	// Snapshot shared state before encoding asynchronously.
	incidentStart := state.startedAt
	incidentDuration := time.Duration(state.endPos-state.startPos) * time.Second / time.Duration(audio.BytesPerSecond)
	ffmpegPath := c.ffmpegPath
	outputDir := c.outputDir
	callback := c.onDumpReady

	state.savedBefore = nil

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

	if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // Dump directory needs to be readable
		result.Error = fmt.Errorf("create output dir: %w", err)
		return result
	}

	result.Filename = dumpFilename(trigger, incidentStart)
	result.FilePath = filepath.Join(outputDir, result.Filename)

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
