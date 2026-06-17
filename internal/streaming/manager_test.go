package streaming

import (
	"bytes"
	"errors"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func validStream() *types.Stream {
	return &types.Stream{
		ID:      "s1",
		Enabled: true,
		Host:    "localhost",
		Port:    9000,
		Codec:   types.CodecPCM,
	}
}

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
			if m.streams[stream.ID] != existing {
				t.Errorf("Start replaced the existing stream entry")
			}
		})
	}
}

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
func TestStatusesNeverMarksListenerStable(t *testing.T) {
	t.Parallel()
	const id = "listener-1"
	m := NewManager("ffmpeg")
	m.streams[id] = &Stream{
		state:     types.ProcessRunning,
		mode:      types.StreamModeListener,
		startTime: time.Now().Add(-2 * types.StableThreshold),
	}
	statuses := m.Statuses(func(string) *types.Stream {
		return &types.Stream{ID: id, Mode: types.StreamModeListener, MaxRetries: 3}
	})
	status := statuses[id]
	if status.State != types.ProcessRunning {
		t.Fatalf("status state = %q, want running", status.State)
	}
	if status.Stable {
		t.Fatal("listener status Stable = true, want false")
	}
}
func TestClassifyStreamExit(t *testing.T) {
	t.Parallel()
	errFailed := errors.New("ffmpeg failed")
	tests := []struct {
		name  string
		mode  types.StreamMode
		err   error
		cause error
		want  streamExitClass
	}{
		{
			name: "caller failure uses retry path",
			mode: types.StreamModeCaller,
			err:  errFailed,
			want: streamExitFailure,
		},
		{
			name: "listener failure uses retry path",
			mode: types.StreamModeListener,
			err:  errFailed,
			want: streamExitFailure,
		},
		{
			name: "listener failure after old relisten window still uses retry path",
			mode: types.StreamModeListener,
			err:  errFailed,
			want: streamExitFailure,
		},
		{
			name: "listener clean exit is treated as encoder failure",
			mode: types.StreamModeListener,
			want: streamExitFailure,
		},
		{
			name:  "intentional stop wins",
			mode:  types.StreamModeListener,
			err:   errFailed,
			cause: errStoppedByUser,
			want:  streamExitIntentionalStop,
		},
		{
			name: "caller clean exit stops normally",
			mode: types.StreamModeCaller,
			want: streamExitNormalStop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyStreamExit(tt.mode, tt.err, tt.cause)
			if got != tt.want {
				t.Fatalf("classifyStreamExit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartReportsNotStartedOnValidationError(t *testing.T) {
	m := NewManager("ffmpeg")
	invalid := &types.Stream{ID: "s1"} // Missing host, codec, and port.
	started, err := m.Start(invalid)
	if err == nil {
		t.Fatal("Start accepted an invalid stream")
	}
	if started {
		t.Error("Start reported started=true on validation failure")
	}
}

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
func TestListenerStartEncoderFailureReleasesFanoutPort(t *testing.T) {
	port := freeUDPPort(t)
	m := NewManager("/nonexistent/ffmpeg-binary-for-test")
	stream := &types.Stream{
		ID:      "listener-1",
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    "127.0.0.1",
		Port:    port,
		Codec:   types.CodecMP3,
	}
	started, err := m.Start(stream)
	if err == nil {
		t.Fatal("Start succeeded with a nonexistent ffmpeg binary")
	}
	if started {
		t.Fatal("Start reported started=true when listener encoder launch failed")
	}
	if _, exists := m.streams[stream.ID]; exists {
		t.Fatal("Start left a placeholder after listener encoder launch failed")
	}
	assertUDPPortAvailable(t, port)
}
func TestListenerFanoutStartFailureRemovesPlaceholder(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("UDP Close() error = %v", err)
		}
	}()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	m := NewManager("unused-ffmpeg")
	stream := &types.Stream{
		ID:      "listener-bind-fail",
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    "127.0.0.1",
		Port:    port,
		Codec:   types.CodecMP3,
	}
	started, err := m.Start(stream)
	if err == nil {
		t.Fatal("Start(listener) succeeded while UDP port was already bound")
	}
	if started {
		t.Fatal("Start(listener) reported started=true after fanout bind failure")
	}
	if _, exists := m.streams[stream.ID]; exists {
		t.Fatal("Start left a placeholder after fanout bind failure")
	}
}
func TestStartListenerWithFFmpegDoesNotRequireSRTProtocol(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	port := freeUDPPort(t)
	m := NewManager(ffmpegPath)
	stream := &types.Stream{
		ID:      "listener-1",
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    "127.0.0.1",
		Port:    port,
		Codec:   types.CodecMP3,
	}
	started, err := m.Start(stream)
	if err != nil {
		t.Fatalf("Start(listener) error = %v", err)
	}
	if !started {
		t.Fatal("Start(listener) reported started=false")
	}
	if err := m.Stop(stream.ID); err != nil {
		t.Fatalf("Stop(listener) error = %v", err)
	}
	assertUDPPortAvailable(t, port)
}
func TestStatusesIncludesListenerEncoderAndClientFields(t *testing.T) {
	t.Parallel()
	const id = "listener-1"
	fanout, err := srtfanout.NewServer(srtfanout.Config{
		Port: 9000,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	m := NewManager("ffmpeg")
	stream := &Stream{
		state:     types.ProcessRunning,
		mode:      types.StreamModeListener,
		startTime: time.Now().Add(-2 * types.StableThreshold),
		fanout:    fanout,
		encoder:   &encoderRun{},
	}
	m.streams[id] = stream
	statuses := m.Statuses(func(string) *types.Stream {
		return &types.Stream{ID: id, Mode: types.StreamModeListener, MaxRetries: 3}
	})
	status := statuses[id]
	if status.Stable {
		t.Fatal("listener status Stable = true, want false")
	}
	if !status.EncoderRunning {
		t.Fatal("listener status EncoderRunning = false, want true")
	}
	if status.ClientCount != 0 {
		t.Fatalf("listener status ClientCount = %d, want 0", status.ClientCount)
	}
}
func TestWriteAudioListenerSkipsWhenNoEncoderRun(t *testing.T) {
	t.Parallel()
	m := NewManager("ffmpeg")
	m.streams["listener-1"] = &Stream{
		state: types.ProcessRunning,
		mode:  types.StreamModeListener,
	}
	if err := m.WriteAudio("listener-1", []byte("pcm")); err != nil {
		t.Fatalf("WriteAudio() error = %v", err)
	}
}
func TestWriteAudioListenerUsesBoundedEncoderQueue(t *testing.T) {
	t.Parallel()
	m := NewManager("ffmpeg")
	run := &encoderRun{audioCh: make(chan []byte, audioBufferSize)}
	stream := &Stream{
		state: types.ProcessRunning,
		mode:  types.StreamModeListener,
	}
	stream.encoder = run
	m.streams["listener-1"] = stream
	for _, chunk := range [][]byte{
		[]byte("one"),
		[]byte("two"),
		[]byte("three"),
		[]byte("four"),
		[]byte("five"),
		[]byte("six"),
	} {
		if err := m.WriteAudio("listener-1", chunk); err != nil {
			t.Fatalf("WriteAudio() error = %v", err)
		}
	}
	if got := len(run.audioCh); got != audioBufferSize {
		t.Fatalf("listener encoder queue len = %d, want %d", got, audioBufferSize)
	}
	if got := string(<-run.audioCh); got != "two" {
		t.Fatalf("oldest queued chunk = %q, want two", got)
	}
	if got := stream.audioDrops.Load(); got != 1 {
		t.Fatalf("audio drops = %d, want 1", got)
	}
}
func TestMonitorListenerEncoderRetriesAndStopsFanoutOnExhaustion(t *testing.T) {
	ffmpegPath := fakeLongRunningExecutable(t)
	port := freeUDPPort(t)
	m := NewManager(ffmpegPath)
	stream := &types.Stream{
		ID:         "listener-retry",
		Enabled:    true,
		Mode:       types.StreamModeListener,
		Host:       "127.0.0.1",
		Port:       port,
		Codec:      types.CodecMP3,
		MaxRetries: 1,
	}
	started, err := m.Start(stream)
	if err != nil {
		t.Fatalf("Start(listener) error = %v", err)
	}
	if !started {
		t.Fatal("Start(listener) reported started=false")
	}
	assertUDPPortUnavailable(t, port)
	m.mu.Lock()
	managed := m.streams[stream.ID]
	managed.backoff = util.NewBackoff(500*time.Millisecond, 500*time.Millisecond)
	m.mu.Unlock()
	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
		if err := m.Stop(stream.ID); err != nil {
			t.Fatalf("Stop(listener) cleanup error = %v", err)
		}
	})
	ctx := staticStreamContext{stream: stream}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.MonitorAndRetry(stream.ID, ctx, stopChan)
	}()
	firstRun := waitListenerRun(t, m, stream.ID, nil)
	if err := firstRun.result.Kill(); err != nil {
		t.Fatalf("first encoder Kill() error = %v", err)
	}
	waitStatus(t, m, stream.ID, func(status types.ProcessStatus) bool {
		return status.State == types.ProcessRunning && !status.EncoderRunning
	}, "listener encoder to stop while fanout remains running")
	assertUDPPortUnavailable(t, port)
	secondRun := waitListenerRun(t, m, stream.ID, firstRun)
	if err := secondRun.result.Kill(); err != nil {
		t.Fatalf("second encoder Kill() error = %v", err)
	}
	waitStatus(t, m, stream.ID, func(status types.ProcessStatus) bool {
		return status.State == types.ProcessError && !status.EncoderRunning
	}, "listener retry exhaustion to mark the stream errored")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MonitorAndRetry did not return after listener retry exhaustion")
	}
	assertUDPPortAvailable(t, port)
}
func TestMonitorListenerEncoderReleasesBlockedWriterAfterStdoutEnds(t *testing.T) {
	origSignalTimeout := encoderRunSignalTimeout
	origKillTimeout := encoderRunKillTimeout
	encoderRunSignalTimeout = 50 * time.Millisecond
	encoderRunKillTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		encoderRunSignalTimeout = origSignalTimeout
		encoderRunKillTimeout = origKillTimeout
	})
	ffmpegPath, closeStdoutPath := fakeBlockedListenerEncoderExecutable(t)
	port := freeUDPPort(t)
	m := NewManager(ffmpegPath)
	stream := &types.Stream{
		ID:         "listener-blocked-writer",
		Enabled:    true,
		Mode:       types.StreamModeListener,
		Host:       "127.0.0.1",
		Port:       port,
		Codec:      types.CodecMP3,
		MaxRetries: 1,
	}
	started, err := m.Start(stream)
	if err != nil {
		t.Fatalf("Start(listener) error = %v", err)
	}
	if !started {
		t.Fatal("Start(listener) reported started=false")
	}
	assertUDPPortUnavailable(t, port)
	m.mu.Lock()
	managed := m.streams[stream.ID]
	managed.backoff = util.NewBackoff(10*time.Millisecond, 10*time.Millisecond)
	m.mu.Unlock()
	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
		if err := m.Stop(stream.ID); err != nil {
			t.Fatalf("Stop(listener) cleanup error = %v", err)
		}
	})
	ctx := staticStreamContext{stream: stream}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.MonitorAndRetry(stream.ID, ctx, stopChan)
	}()
	run := waitListenerRun(t, m, stream.ID, nil)
	if err := m.WriteAudio(stream.ID, bytes.Repeat([]byte{1}, 8*1024*1024)); err != nil {
		t.Fatalf("WriteAudio() error = %v", err)
	}
	waitForListenerWriterToTakeAudio(t, run)
	if err := os.WriteFile(closeStdoutPath, []byte("close"), 0o600); err != nil {
		t.Fatalf("WriteFile(close stdout marker) error = %v", err)
	}
	secondRun := waitListenerRun(t, m, stream.ID, run)
	if secondRun == run {
		t.Fatal("listener retry reused the failed encoder run")
	}
	statuses := m.Statuses(func(string) *types.Stream {
		return stream
	})
	status := statuses[stream.ID]
	if status.State != types.ProcessRunning {
		t.Fatalf("listener state = %q, want %q", status.State, types.ProcessRunning)
	}
	if !status.EncoderRunning {
		t.Fatal("listener encoder is not running after retry")
	}
	if status.RetryCount != 1 {
		t.Fatalf("listener retry count = %d, want 1", status.RetryCount)
	}
	select {
	case <-done:
		t.Fatal("MonitorAndRetry returned while listener retry should keep monitoring")
	default:
	}
	assertUDPPortUnavailable(t, port)
}
func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ResolveUDPAddr() error = %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("UDP Close() error = %v", err)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}
func fakeLongRunningExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	script := "#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"
	//nolint:gosec // Test helper must be executable and lives in t.TempDir().
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake ffmpeg) error = %v", err)
	}
	return path
}
func fakeBlockedListenerEncoderExecutable(t *testing.T) (path, closeStdoutPath string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "fake-ffmpeg")
	closeStdoutPath = filepath.Join(dir, "close-stdout")
	t.Setenv("FAKE_FFMPEG_CLOSE_STDOUT", closeStdoutPath)
	script := `#!/bin/sh
trap 'exit 0' TERM INT
while [ ! -f "$FAKE_FFMPEG_CLOSE_STDOUT" ]; do
	sleep 0.01
done
rm -f "$FAKE_FFMPEG_CLOSE_STDOUT"
exec 1>&-
while :; do
	sleep 1
done
`
	//nolint:gosec // Test helper must be executable and lives in t.TempDir().
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake ffmpeg) error = %v", err)
	}
	return path, closeStdoutPath
}
func assertUDPPortAvailable(t *testing.T, port int) {
	t.Helper()
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("UDP port %d is not available after cleanup: %v", port, err)
	}
	_ = conn.Close()
}
func assertUDPPortUnavailable(t *testing.T, port int) {
	t.Helper()
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("UDP Close() error = %v", closeErr)
		}
		t.Fatalf("UDP port %d is available while fanout should own it", port)
	}
}
func waitListenerRun(t *testing.T, m *Manager, streamID string, previous *encoderRun) *encoderRun {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		stream := m.streams[streamID]
		m.mu.RUnlock()
		if stream != nil {
			stream.encoderMu.RLock()
			run := stream.encoder
			stream.encoderMu.RUnlock()
			if run != nil && run != previous {
				return run
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for listener encoder run after %p", previous)
	return nil
}
func waitForListenerWriterToTakeAudio(t *testing.T, run *encoderRun) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(run.audioCh) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for listener writer to take queued audio")
}
func waitStatus(
	t *testing.T,
	m *Manager,
	streamID string,
	match func(types.ProcessStatus) bool,
	desc string,
) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		statuses := m.Statuses(func(string) *types.Stream {
			return &types.Stream{ID: streamID, Mode: types.StreamModeListener, MaxRetries: 1}
		})
		status, ok := statuses[streamID]
		if ok && match(status) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

type staticStreamContext struct {
	stream *types.Stream
}

func (c staticStreamContext) Stream(streamID string) *types.Stream {
	if c.stream != nil && c.stream.ID == streamID {
		return c.stream
	}
	return nil
}
func (c staticStreamContext) IsRunning() bool {
	return true
}
