package encoder

import (
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const testStreamRestartDelay = 200 * time.Millisecond

// TestStartRejectsStateStopping verifies that Start() returns ErrAlreadyRunning when the
// encoder is in StateStopping. This guards against the regression where the MaxRetries
// exit path set state = StateStopped before cleanup finished, allowing a concurrent
// Start() to race the in-progress cleanup (StopAll, silenceDumpManager.Stop, DrainLogs).
// The fix transitions through StateStopping during cleanup; Start() must block that state.
func TestStartRejectsStateStopping(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	// Set a non-empty AudioInput so Start() reaches the state guard rather than
	// returning ErrNoAudioInput before it gets there. All required silence fields
	// must be valid because ApplySettings now validates before applying.
	if err := cfg.ApplySettings(&config.SettingsUpdate{
		AudioInput:                  "test-device",
		SilenceThreshold:            config.DefaultSilenceThreshold,
		SilenceDurationMs:           config.DefaultSilenceDurationMs,
		SilenceRecoveryMs:           config.DefaultSilenceRecoveryMs,
		PeakHoldMs:                  config.DefaultPeakHoldMs,
		ChannelImbalanceThreshold:   config.DefaultChannelImbalanceThreshold,
		ChannelImbalanceDurationMs:  config.DefaultChannelImbalanceDurationMs,
		ChannelImbalanceRecoveryMs:  config.DefaultChannelImbalanceRecoveryMs,
		RecordingMaxDurationMinutes: config.DefaultRecordingMaxDurationMinutes,
	}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	e := &Encoder{
		config: cfg,
		state:  types.StateStopping,
	}

	if err := e.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Start() in StateStopping = %v, want ErrAlreadyRunning", err)
	}
}

func TestDelayedStarterDoesNotStartManagersAfterQuickSourceExit(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitForCondition(t, time.Second, "source retry after quick exit", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.state == types.StateStarting && e.retryCount > 0
	})
	time.Sleep(3 * testStreamRestartDelay)

	if e.recordingManager.IsRunning() {
		t.Fatal("recording manager was started by a stale delayed source starter")
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatal("silence dump manager was started by a stale delayed source starter")
	}
}

func TestStaleStarterDoesNotStartManagersForNewRun(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	e.mu.Lock()
	e.state = types.StateRunning
	e.stopChan = make(chan struct{})
	e.sourceStdout = pr
	e.sourceRunID = 2
	e.mu.Unlock()

	e.startEnabledStreams(1)

	if e.recordingManager.IsRunning() {
		t.Fatal("stale run-1 starter started recording manager while run 2 was active")
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatal("stale run-1 starter started silence dump manager while run 2 was active")
	}
}

func TestDelayedStarterReturnsWhenStopChanClosed(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	closedStopChan := make(chan struct{})
	close(closedStopChan)

	e.mu.Lock()
	e.state = types.StateRunning
	e.stopChan = make(chan struct{})
	e.sourceStdout = pr
	e.sourceRunID = 1
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.startEnabledStreamsAfterDelay(1, closedStopChan, 500*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("delayed starter did not return after stopChan closed")
	}

	if e.recordingManager.IsRunning() {
		t.Fatal("closed stopChan delayed starter started recording manager")
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatal("closed stopChan delayed starter started silence dump manager")
	}
}

func TestStopBeforeStreamDelayCancelsDelayedStarter(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "capture-ready")
	e := newSourceLifecycleTestEncoder(t, "sleep", readyPath)

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitForCondition(t, time.Second, "source running", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.state == types.StateRunning && e.sourceCmd != nil && e.sourceRunID > 0
	})
	waitForCondition(t, testStreamRestartDelay/2, "capture helper signal readiness", func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	})

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	time.Sleep(3 * testStreamRestartDelay)

	if e.recordingManager.IsRunning() {
		t.Fatal("recording manager was revived after Stop() canceled the delayed starter")
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatal("silence dump manager was revived after Stop() canceled the delayed starter")
	}
}

func TestSourceMaxRetryExhaustionStopsRecordingManager(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	if err := e.recordingManager.Start(); err != nil {
		t.Fatalf("recordingManager.Start() error = %v", err)
	}
	e.silenceDumpManager.Start()

	e.mu.Lock()
	e.state = types.StateStarting
	e.stopChan = make(chan struct{})
	e.retryCount = types.MaxRetries - 1
	e.backoff.Reset()
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.runSourceLoop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSourceLoop did not stop after source retries were exhausted")
	}
	time.Sleep(3 * testStreamRestartDelay)

	if got := e.State(); got != types.StateStopped {
		t.Fatalf("State() = %q, want %q", got, types.StateStopped)
	}
	if e.recordingManager.IsRunning() {
		t.Fatal("recording manager is still running after source retries were exhausted")
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatal("silence dump manager is still running after source retries were exhausted")
	}
}

func TestCloseIdempotent(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))

	logger, err := eventlog.NewLogger(filepath.Join(t.TempDir(), "encoder.jsonl"))
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}

	e := &Encoder{
		eventLogger:       logger,
		alertOrchestrator: notify.NewAlertOrchestrator(cfg, notify.NewDispatcher()),
	}

	if err := e.Close(); err != nil {
		t.Fatalf("first Close() failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}
}

func TestStreamStatusesReportsSRTUnsupported(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	stream := types.Stream{
		ID:      "stream-1",
		Enabled: true,
		Host:    "stream.example.com",
		Port:    9000,
		Codec:   types.CodecMP3,
	}

	e := &Encoder{
		config:        cfg,
		ffmpegPath:    "ffmpeg",
		srtAvailable:  false,
		streamManager: streaming.NewManager("ffmpeg"),
	}

	statuses := e.StreamStatuses([]types.Stream{stream})
	status := statuses[stream.ID]
	if status.State != types.ProcessError {
		t.Fatalf("status state = %q, want error", status.State)
	}
	if status.Error != ErrSRTUnsupported.Error() {
		t.Fatalf("status error = %q, want %q", status.Error, ErrSRTUnsupported.Error())
	}
}

func TestStreamStatusesReportsSRTProbeErrorSeparately(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	stream := types.Stream{
		ID:      "stream-1",
		Enabled: true,
		Host:    "stream.example.com",
		Port:    9000,
		Codec:   types.CodecMP3,
	}

	e := &Encoder{
		config:        cfg,
		ffmpegPath:    "ffmpeg",
		srtAvailable:  false,
		srtProbeError: errors.New("probe timed out"),
		streamManager: streaming.NewManager("ffmpeg"),
	}

	statuses := e.StreamStatuses([]types.Stream{stream})
	status := statuses[stream.ID]
	if status.State != types.ProcessError {
		t.Fatalf("status state = %q, want error", status.State)
	}
	if status.Error != ErrSRTUnverified.Error() {
		t.Fatalf("status error = %q, want %q", status.Error, ErrSRTUnverified.Error())
	}
}

func TestStreamStatusesDoesNotRequireSRTForListener(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	stream := types.Stream{
		ID:      "listener-1",
		Enabled: true,
		Mode:    types.StreamModeListener,
		Host:    "127.0.0.1",
		Port:    9000,
		Codec:   types.CodecMP3,
	}

	e := &Encoder{
		config:        cfg,
		ffmpegPath:    "ffmpeg",
		srtAvailable:  false,
		streamManager: streaming.NewManager("ffmpeg"),
	}

	statuses := e.StreamStatuses([]types.Stream{stream})
	status := statuses[stream.ID]
	if status.State != types.ProcessStopped {
		t.Fatalf("listener status state = %q, want stopped", status.State)
	}
	if status.Error != "" {
		t.Fatalf("listener status error = %q, want empty", status.Error)
	}
}

func TestStartStreamReportsSRTProbeErrorSeparately(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Streaming.Streams = []types.Stream{
		{
			ID:      "stream-1",
			Enabled: true,
			Host:    "stream.example.com",
			Port:    9000,
			Codec:   types.CodecMP3,
		},
	}

	e := &Encoder{
		config:        cfg,
		state:         types.StateRunning,
		stopChan:      make(chan struct{}),
		srtAvailable:  false,
		srtProbeError: errors.New("probe timed out"),
		streamManager: streaming.NewManager("ffmpeg"),
	}

	if err := e.StartStream("stream-1"); !errors.Is(err, ErrSRTUnverified) {
		t.Fatalf("StartStream() error = %v, want ErrSRTUnverified", err)
	}
}

func TestStartStreamDoesNotRequireSRTForListener(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Streaming.Streams = []types.Stream{
		{
			ID:      "listener-1",
			Enabled: true,
			Mode:    types.StreamModeListener,
			Host:    "127.0.0.1",
			Port:    freeUDPPort(t),
			Codec:   types.CodecMP3,
		},
	}

	e := &Encoder{
		config:        cfg,
		state:         types.StateRunning,
		stopChan:      make(chan struct{}),
		srtAvailable:  false,
		srtProbeError: errors.New("probe timed out"),
		streamManager: streaming.NewManager("/nonexistent/ffmpeg-binary-for-test"),
	}

	err := e.StartStream("listener-1")
	if err == nil {
		t.Fatal("StartStream() unexpectedly succeeded with nonexistent ffmpeg")
	}
	if errors.Is(err, ErrSRTUnsupported) || errors.Is(err, ErrSRTUnverified) {
		t.Fatalf("StartStream() error = %v, want real listener start error instead of SRT sentinel", err)
	}
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

func newSourceLifecycleTestEncoder(t *testing.T, helperMode string, helperArgs ...string) *Encoder {
	t.Helper()

	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Audio.Input = "test-device"
	cfg.Streaming.Streams = []types.Stream{}
	cfg.Recording.Recorders = []types.Recorder{}
	cfg.Recording.MaxDurationMinutes = config.DefaultRecordingMaxDurationMinutes

	recordingManager, err := recording.NewManager("", t.TempDir(), config.DefaultRecordingMaxDurationMinutes, nil)
	if err != nil {
		t.Fatalf("recording.NewManager() error = %v", err)
	}

	e := &Encoder{
		config:              cfg,
		buildCaptureCommand: helperCaptureCommand(helperMode, helperArgs...),
		streamRestartDelay:  testStreamRestartDelay,
		streamManager:       streaming.NewManager(""),
		recordingManager:    recordingManager,
		silenceDumpManager:  silencedump.NewManager("", 0, false, 0, nil),
		state:               types.StateStopped,
		backoff:             util.NewBackoff(types.InitialRetryDelay, types.MaxRetryDelay),
		silenceDetect:       audio.NewSilenceDetector(),
		imbalanceDetect:     audio.NewImbalanceDetector(),
		alertOrchestrator:   notify.NewAlertOrchestrator(cfg, notify.NewDispatcher()),
		peakHolder:          audio.NewPeakHolder(),
	}

	t.Cleanup(func() {
		_ = e.Stop()
		if e.recordingManager != nil {
			_ = e.recordingManager.Stop()
		}
		if e.silenceDumpManager != nil {
			e.silenceDumpManager.Stop()
		}
	})

	return e
}

func helperCaptureCommand(mode string, extraArgs ...string) func(string, string) (string, []string, error) {
	return func(_, _ string) (string, []string, error) {
		args := []string{"-test.run=TestEncoderCaptureHelperProcess", "--", mode}
		args = append(args, extraArgs...)
		return os.Args[0], args, nil
	}
}

func TestEncoderCaptureHelperProcess(t *testing.T) {
	helperArgs := []string{}
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			helperArgs = os.Args[i+1:]
			break
		}
	}
	if len(helperArgs) == 0 {
		return
	}

	mode := helperArgs[0]
	switch mode {
	case "exit":
		os.Exit(1)
	case "sleep":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, util.ShutdownSignals()...)
		if len(helperArgs) > 1 {
			//nolint:gosec // G703: helperArgs[1] is a temp-file path created by this test process.
			if err := os.WriteFile(helperArgs[1], []byte("ready"), 0o600); err != nil {
				signal.Stop(signals)
				os.Exit(2)
			}
		}

		select {
		case <-signals:
		case <-time.After(time.Minute):
		}
		signal.Stop(signals)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
