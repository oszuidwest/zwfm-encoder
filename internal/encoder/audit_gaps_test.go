//go:build audit

package encoder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// A stream edited while the input is being reopened must not keep running
// with its old settings once the next source run starts.
func TestAuditStreamDisabledDuringInputOutageStops(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	fake := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 3600\n"), 0o755); err != nil { //nolint:gosec // The fake FFmpeg must be executable.
		t.Fatal(err)
	}
	e.streamManager = streaming.NewManager(fake)
	e.srtAvailable = true
	stream := types.Stream{ID: "s1", Enabled: true, Host: "localhost", Port: 9000, Codec: types.CodecPCM}
	e.config.Streaming.Streams = []types.Stream{stream}

	// Run 1 is live and the stream runs.
	e.mu.Lock()
	e.state, e.sourceRunID = types.StateRunning, 1
	e.stopChan, e.sourceStdout = make(chan struct{}), strings.NewReader("")
	e.mu.Unlock()
	if err := e.StartStream("s1"); err != nil {
		t.Fatal(err)
	}

	// Run 1 failed: runSourceLoop sets StateStarting while it backs off (encoder.go:904).
	e.mu.Lock()
	e.state, e.sourceStdout = types.StateStarting, nil
	e.mu.Unlock()

	// The operator disables the stream; api.go handleUpdateStream does exactly this.
	stream.Enabled = false
	if err := e.config.UpdateStream(&stream); err != nil {
		t.Fatal(err)
	}
	e.RestartStream("s1")

	// Run 2 publishes and its delayed starter runs.
	e.mu.Lock()
	e.state, e.sourceRunID = types.StateRunning, 2
	e.sourceStdout = strings.NewReader("")
	e.mu.Unlock()
	time.Sleep(types.StreamRestartDelay + 100*time.Millisecond)
	e.startEnabledStreams(2)

	st := e.streamManager.Statuses(e.config.Stream)["s1"]
	if st.State == types.ProcessRunning {
		t.Fatalf("disabled stream still has a running FFmpeg process (state %q)", st.State)
	}
}
