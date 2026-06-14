package streaming

import (
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// validStream returns a stream that reaches Start's state checks.
func validStream() *types.Stream {
	return &types.Stream{
		ID:      "s1",
		Enabled: true,
		Host:    "localhost",
		Port:    9000,
		Codec:   types.CodecPCM,
	}
}

// TestStartReportsNotStartedWhenAlreadyActive verifies active streams do not
// spawn duplicate monitors.
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
			// No placeholder swap: the existing process still owns the stream.
			if m.streams[stream.ID] != existing {
				t.Errorf("Start replaced the existing stream entry")
			}
		})
	}
}

// TestStartRelaunchesErroredOrStoppedEntry verifies retryable states attempt a
// new process instead of taking the already-active shortcut.
func TestStartRelaunchesErroredOrStoppedEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state types.ProcessState
	}{
		{"error", types.ProcessError},
		{"stopped", types.ProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager("/nonexistent/ffmpeg-binary-for-test")
			stream := validStream()
			m.streams[stream.ID] = &Stream{state: tc.state}

			started, err := m.Start(stream)
			if started {
				t.Fatalf("Start reported started=true despite a failed relaunch")
			}
			if err == nil {
				t.Errorf("Start took the already-active shortcut for a %s entry; "+
					"it must fall through to a relaunch", tc.name)
			}
		})
	}
}

// TestMaybeEmitStableOnlyForSameRunningInstance verifies fast restarts cannot
// inherit another instance's stable event.
func TestMaybeEmitStableOnlyForSameRunningInstance(t *testing.T) {
	const id = "s1"

	for _, tc := range []struct {
		name     string
		setup    func(m *Manager, started *Stream)
		wantEmit bool
	}{
		{
			name: "same running instance emits",
			setup: func(m *Manager, started *Stream) {
				m.streams[id] = started
			},
			wantEmit: true,
		},
		{
			name: "different running instance does not emit",
			setup: func(m *Manager, _ *Stream) {
				// Same ID, different owner.
				m.streams[id] = &Stream{state: types.ProcessRunning}
			},
			wantEmit: false,
		},
		{
			name: "same instance no longer running does not emit",
			setup: func(m *Manager, started *Stream) {
				started.state = types.ProcessStopped
				m.streams[id] = started
			},
			wantEmit: false,
		},
		{
			name: "id removed does not emit",
			setup: func(_ *Manager, _ *Stream) {
				// Stopped/removed streams have no current owner.
			},
			wantEmit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager("ffmpeg")
			var events []string
			m.SetEventCallback(func(_, _, event, _, _ string, _, _ int) {
				events = append(events, event)
			}, nil)

			started := &Stream{state: types.ProcessRunning}
			tc.setup(m, started)

			m.maybeEmitStable(id, started)

			emitted := slices.Contains(events, "stream_stable")
			if emitted != tc.wantEmit {
				t.Errorf("maybeEmitStable emitted=%v, want %v (events=%v)",
					emitted, tc.wantEmit, events)
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
