//go:build audit

package silencedump

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
)

// Stop must not return while an encode it owns is still running (#494).
func TestAuditStopWaitsForRunningEncode(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	// Consume stdin, then take a while like a real encode.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nsleep 1\n"), 0o755); err != nil { //nolint:gosec // The fake FFmpeg must be executable.
		t.Fatal(err)
	}
	var delivered atomic.Bool
	m := NewManager(fake, 0, true, 0, func(*EncodeResult) { delivered.Store(true) })
	m.outputDir = filepath.Join(dir, "out")
	m.capturer.outputDir = m.outputDir
	m.Start()

	m.WriteAudio(make([]byte, audio.BytesPerSecond))
	m.capturer.OnIncidentStart(TriggerSilence, 1)
	m.WriteAudio(make([]byte, audio.BytesPerSecond))
	m.capturer.OnIncidentRecover(TriggerSilence, 1, time.Second, 0)
	chunk := make([]byte, audio.BytesPerSecond/10)
	for range (afterSeconds + 1) * 10 {
		m.WriteAudio(chunk) // Finalizes and launches the encode goroutine.
	}

	m.Stop()
	if !delivered.Load() {
		deadline := time.Now().Add(5 * time.Second)
		for !delivered.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("Stop returned while a dump encode was still running (delivered later: %t)", delivered.Load())
	}
}
