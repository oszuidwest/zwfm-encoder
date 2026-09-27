package streaming

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

type streamEventRecorder struct {
	mu     sync.Mutex
	events []streamEventForTest
}

func recordStreamEvents(m *Manager) *streamEventRecorder {
	r := &streamEventRecorder{}
	m.SetEventCallback(func(_, _, mode, event, message, _ string, _, _ int) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, streamEventForTest{mode: mode, event: event, message: message})
	}, nil)
	return r
}

func (r *streamEventRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, len(r.events))
	for i := range r.events {
		names[i] = r.events[i].event
	}
	return names
}

func (r *streamEventRecorder) waitFor(t *testing.T, eventName string, want int) {
	t.Helper()
	waitForStreamEventCount(t, &r.mu, &r.events, eventName, want)
}

func waitMonitorDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("MonitorAndRetry did not return after Stop")
	}
}

func TestStopEmitsOneStoppedEventForIdleStates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state types.ProcessState
		mode  types.StreamMode
	}{
		{"caller starting", types.ProcessStarting, types.StreamModeCaller},
		{"caller stopped", types.ProcessStopped, types.StreamModeCaller},
		{"caller error", types.ProcessError, types.StreamModeCaller},
		{"listener starting", types.ProcessStarting, types.StreamModeListener},
		{"listener error", types.ProcessError, types.StreamModeListener},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := NewManager("ffmpeg")
			events := recordStreamEvents(m)
			m.streams["s1"] = &Stream{state: tc.state, mode: tc.mode}

			for range 2 {
				if err := m.Stop("s1"); err != nil {
					t.Fatalf("Stop() error = %v", err)
				}
			}

			if got := events.names(); len(got) != 1 || got[0] != "stream_stopped" {
				t.Fatalf("events = %v, want exactly one stream_stopped", got)
			}
			if got := events.events[0].mode; got != string(tc.mode) {
				t.Fatalf("stream_stopped mode = %q, want %q", got, tc.mode)
			}
			if m.current("s1") != nil {
				t.Fatal("Stop left the stream entry in the manager")
			}
		})
	}
}

func TestStopSkipsStreamAlreadyStopping(t *testing.T) {
	t.Parallel()

	m := NewManager("ffmpeg")
	events := recordStreamEvents(m)
	entry := &Stream{state: types.ProcessStopping, mode: types.StreamModeCaller}
	m.streams["s1"] = entry

	if err := m.Stop("s1"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := events.names(); len(got) != 0 {
		t.Fatalf("events = %v, want none while another Stop owns shutdown", got)
	}
	if m.current("s1") != entry {
		t.Fatal("Stop removed an entry owned by a concurrent Stop")
	}
}

func TestStopRunningCallerEndsMonitorWithoutRetry(t *testing.T) {
	m := NewManager(fakeLongRunningExecutable(t))
	events := recordStreamEvents(m)
	stream := validStream()
	if started, err := m.Start(stream); err != nil || !started {
		t.Fatalf("Start() = %v, %v; want started", started, err)
	}

	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The config stays present, as it does while a delete stops the stream.
		m.MonitorAndRetry(stream.ID, staticStreamContext{stream: stream}, stopChan)
	}()

	if err := m.Stop(stream.ID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	waitMonitorDone(t, done)

	if got := events.names(); len(got) != 2 || got[0] != "stream_started" || got[1] != "stream_stopped" {
		t.Fatalf("events = %v, want [stream_started stream_stopped]", got)
	}
	if m.current(stream.ID) != nil {
		t.Fatal("stream was restarted after Stop")
	}
}

func TestStopDuringCallerRetryWaitCancelsRestart(t *testing.T) {
	m := NewManager(writeFakeFFmpeg(t, "#!/bin/sh\nexit 1\n"))
	events := recordStreamEvents(m)
	stream := validStream()
	if started, err := m.Start(stream); err != nil || !started {
		t.Fatalf("Start() = %v, %v; want started", started, err)
	}
	m.mu.Lock()
	m.streams[stream.ID].backoff = util.NewBackoff(300*time.Millisecond, 300*time.Millisecond)
	m.mu.Unlock()

	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.MonitorAndRetry(stream.ID, staticStreamContext{stream: stream}, stopChan)
	}()

	events.waitFor(t, "stream_retry", 1)
	if err := m.Stop(stream.ID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	waitMonitorDone(t, done)

	want := []string{"stream_started", "stream_error", "stream_retry", "stream_stopped"}
	if got := events.names(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if m.current(stream.ID) != nil {
		t.Fatal("retry monitor restarted the stream after Stop")
	}
}

func TestStopDuringListenerRetryWaitCancelsRestart(t *testing.T) {
	ffmpegPath := fakeLongRunningExecutable(t)
	port := freeUDPPort(t)
	m := NewManager(ffmpegPath)
	events := recordStreamEvents(m)
	stream := &types.Stream{
		ID:      "listener-stop",
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    "127.0.0.1",
		Port:    port,
		Codec:   types.CodecMP3,
	}
	if started, err := m.Start(stream); err != nil || !started {
		t.Fatalf("Start(listener) = %v, %v; want started", started, err)
	}
	m.mu.Lock()
	m.streams[stream.ID].backoff = util.NewBackoff(300*time.Millisecond, 300*time.Millisecond)
	m.mu.Unlock()

	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.MonitorAndRetry(stream.ID, staticStreamContext{stream: stream}, stopChan)
	}()

	run := waitListenerRun(t, m, stream.ID, nil)
	if err := run.result.Kill(); err != nil {
		t.Fatalf("encoder Kill() error = %v", err)
	}
	events.waitFor(t, "stream_retry", 1)
	if err := m.Stop(stream.ID); err != nil {
		t.Fatalf("Stop(listener) error = %v", err)
	}
	waitMonitorDone(t, done)

	want := []string{"stream_started", "stream_error", "stream_retry", "stream_stopped"}
	if got := events.names(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if m.current(stream.ID) != nil {
		t.Fatal("listener entry remains after Stop")
	}
	assertUDPPortAvailable(t, port)
}
