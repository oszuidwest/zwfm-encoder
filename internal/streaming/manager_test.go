package streaming

import (
	"slices"
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

// TestStartRelaunchesErroredOrStoppedEntry guards the state-specific early
// return: only ProcessRunning and ProcessStarting count as already-active. An
// errored or stopped entry must fall through to a real (re)launch - the path
// MonitorAndRetry drives on every retry. Broadening the guard to "any existing
// entry" would silently kill retry-driven restarts, and the already-active
// tests above would still pass. Here the relaunch is forced to fail with a
// bogus ffmpeg path, so a non-nil error is precisely what proves Start did not
// take the (false, nil) already-active shortcut.
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

// TestMaybeEmitStableOnlyForSameRunningInstance is the regression guard for the
// premature stream_stable bug (finding 8, #298). The deferred stable goroutine
// captures the instance it launched and must emit only when that same instance
// is still the current, running stream. A fast restart that replaces the
// instance within StableThreshold must not produce a stable event for the
// seconds-old replacement. Checking only the ID (the pre-fix behavior) would
// emit on the "different running instance" case below.
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
				// A fast restart replaced the launched instance: a different
				// *Stream now owns the ID and is running, but it is not the
				// instance the stable goroutine was spawned for.
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
				// Leave the map empty: the stream was stopped/removed.
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
