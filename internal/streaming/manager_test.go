package streaming

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// validStream returns a stream that passes Validate so Start proceeds past the
// validation gate into the running-state checks.
func validStream() *types.Stream {
	return &types.Stream{
		ID:      "s1",
		Enabled: true,
		Host:    "localhost",
		Port:    9000,
		Codec:   types.CodecPCM,
	}
}

// TestStartReportsNotStartedWhenAlreadyActive is the regression guard for the
// duplicate-MonitorAndRetry bug (finding 3, #293). Start must report
// started=false with no error when the stream is already running or starting,
// so the caller does not spawn a second monitor goroutine. The reachable path
// is startEnabledStreams re-calling StartStream on every source retry while the
// stream FFmpeg processes keep running.
func TestStartReportsNotStartedWhenAlreadyActive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state types.ProcessState
	}{
		{"running", types.ProcessRunning},
		{"starting", types.ProcessStarting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager("ffmpeg")
			stream := validStream()
			existing := &Stream{state: tc.state}
			m.streams[stream.ID] = existing

			started, err := m.Start(stream)
			if err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			if started {
				t.Errorf("Start reported started=true for an already-%s stream; "+
					"the caller would spawn a duplicate monitor", tc.name)
			}
			// The existing entry must be left untouched - no placeholder swap.
			if m.streams[stream.ID] != existing {
				t.Errorf("Start replaced the existing stream entry")
			}
		})
	}
}

// TestStartReportsNotStartedOnValidationError verifies an invalid stream yields
// started=false so the caller does not monitor a stream that never launched.
func TestStartReportsNotStartedOnValidationError(t *testing.T) {
	m := NewManager("ffmpeg")
	invalid := &types.Stream{ID: "s1"} // missing host, codec, and port

	started, err := m.Start(invalid)
	if err == nil {
		t.Fatal("Start accepted an invalid stream")
	}
	if started {
		t.Error("Start reported started=true on validation failure")
	}
}

// TestStartReportsNotStartedWhenProcessLaunchFails verifies that when FFmpeg
// cannot be launched, Start returns started=false and removes its placeholder
// so no stale entry or orphaned monitor is left behind.
func TestStartReportsNotStartedWhenProcessLaunchFails(t *testing.T) {
	m := NewManager("/nonexistent/ffmpeg-binary-for-test")
	stream := validStream()

	started, err := m.Start(stream)
	if err == nil {
		t.Fatal("Start succeeded with a nonexistent ffmpeg binary")
	}
	if started {
		t.Error("Start reported started=true when the process failed to launch")
	}
	if _, exists := m.streams[stream.ID]; exists {
		t.Error("Start left a placeholder entry after a failed launch")
	}
}
