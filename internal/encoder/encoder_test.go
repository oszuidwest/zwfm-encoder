package encoder

import (
	"context"
	"errors"
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testStreamRestartDelay = 200 * time.Millisecond

const nonexistentFFmpeg = "/nonexistent/ffmpeg-binary-for-test"

func TestStopTimeoutPreventsOverlappingSourceLoops(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	e.config.Audio.Input = "dante://studio-tx/Left/Right"
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var opens atomic.Int32
	e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
		reader, writer := io.Pipe()
		context.AfterFunc(ctx, func() { _ = writer.Close() })
		if opens.Add(1) == 1 {
			return fakeDanteSource{reader, release}, nil
		}
		return fakeDanteSource{reader, ctx.Done()}, nil
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, "first source running", func() bool {
		return e.State() == types.StateRunning
	})
	if err := e.Stop(); err == nil || !strings.Contains(err.Error(), "shutdown timeout") {
		t.Fatalf("Stop() error = %v, want shutdown timeout", err)
	}
	if state := e.State(); state != types.StateStopping {
		t.Errorf("State after timeout = %q, want stopping", state)
	}
	if err := e.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Start while old source is alive = %v, want ErrAlreadyRunning", err)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("sources opened while stopping = %d, want 1", got)
	}
	releaseOnce.Do(func() { close(release) })
	waitForCondition(t, time.Second, "late source shutdown completes", func() bool {
		return e.State() == types.StateStopped
	})
	if err := e.Start(); err != nil {
		t.Fatalf("Start after old loop finished: %v", err)
	}
	waitForCondition(t, time.Second, "new source running", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return opens.Load() == 2 && e.state == types.StateRunning && e.sourceCancel != nil && e.sourceStdout != nil
	})
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop new source: %v", err)
	}
}

func TestStartRejectsStateStopping(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
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
		config:     cfg,
		ffmpegPath: "ffmpeg",
		state:      types.StateStopping,
	}
	if err := e.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Start() in StateStopping = %v, want ErrAlreadyRunning", err)
	}
}
func TestDelayedStarterDoesNotStartManagersAfterQuickSourceExit(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	// Reaching retry within this budget proves the delayed starter is stale.
	const starterDelay = time.Second
	e.streamRestartDelay = starterDelay
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, starterDelay/2, "source retry after quick exit", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.state == types.StateStarting && e.retryCount > 0
	})
	time.Sleep(starterDelay + testStreamRestartDelay)
	assertManagersStopped(t, e, "stale delayed source starter ran after a quick source exit")
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
	assertManagersStopped(t, e, "stale run-1 starter ran while run 2 was active")
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
	assertManagersStopped(t, e, "closed stopChan delayed starter ran")
}
func TestStopBeforeStreamDelayCancelsDelayedStarter(t *testing.T) {
	// A pipe source cannot spend the pre-delay window starting a subprocess.
	e := newSourceLifecycleTestEncoder(t, "exit")
	e.config.Audio.Input = "dante://studio-tx/Left/Right"
	e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
		reader, writer := io.Pipe()
		context.AfterFunc(ctx, func() { _ = writer.Close() })
		return fakeDanteSource{reader, ctx.Done()}, nil
	}
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, time.Second, "source running", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.state == types.StateRunning && e.sourceCancel != nil && e.sourceStdout != nil && e.sourceRunID > 0
	})
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	time.Sleep(3 * testStreamRestartDelay)
	assertManagersStopped(t, e, "managers revived after Stop() canceled the delayed starter")
}

func TestNativeDanteSourceUsesEncoderLifecycle(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	e.config.Audio.Input = "dante://studio-tx/Left/Right"
	e.openDanteSource = func(ctx context.Context, _ string) (source, error) {
		reader, writer := io.Pipe()
		context.AfterFunc(ctx, func() { _ = writer.Close() }) // Cancellation must unblock reads.
		return fakeDanteSource{reader, ctx.Done()}, nil
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, time.Second, "native Dante source running", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.state == types.StateRunning && e.sourceCancel != nil && e.sourceStdout != nil
	})
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := e.State(); got != types.StateStopped {
		t.Fatalf("State() = %q, want %q", got, types.StateStopped)
	}
}
func TestSourceKeepsRetryingAndReportsLossOnce(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	logPath := attachEventLog(t, e)
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// More than 15 attempts: the source loop has no retry limit.
	waitForCondition(t, 20*time.Second, "source retries past 15 attempts", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.retryCount > 15
	})
	status := e.Status()
	if status.State == types.StateStopped {
		t.Fatal("encoder stopped while the input was unavailable; want it to keep retrying")
	}
	if status.LastError == "" {
		t.Fatal("Status().LastError is empty while the input is unavailable")
	}

	if got := countEvents(t, e, logPath, eventlog.InputLost); got != 1 {
		t.Fatalf("input_lost events = %d, want exactly 1 for one outage", got)
	}
	if got := countEvents(t, e, logPath, eventlog.InputRestored); got != 0 {
		t.Fatalf("input_restored events = %d, want 0 while the input stays unavailable", got)
	}
}

func TestSourceRecoveryReportsInputRestored(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	logPath := attachEventLog(t, e)
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)
	e.inputStableDelay = 300 * time.Millisecond
	var attempts atomic.Int32
	e.buildCaptureCommand = func(device, ffmpegPath string) (string, []string, error) {
		mode := "pcm"
		if attempts.Add(1) <= 3 {
			mode = "exit"
		}
		return helperCaptureCommand(mode)(device, ffmpegPath)
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, 10*time.Second, "input_restored logged", func() bool {
		return countEvents(t, e, logPath, eventlog.InputRestored) == 1
	})
	if got := e.Status().SourceRetryCount; got != 0 {
		t.Fatalf("SourceRetryCount = %d after recovery, want 0", got)
	}
	if got := countEvents(t, e, logPath, eventlog.InputLost); got != 1 {
		t.Fatalf("input_lost events = %d, want 1", got)
	}
	audioEvents, _, err := eventlog.ReadLast(logPath, 10, 0, eventlog.FilterAudio)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	// The helper's silent PCM may also raise silence events; only input events matter here.
	var events []eventlog.Event
	for _, event := range audioEvents {
		if event.Type == eventlog.InputLost || event.Type == eventlog.InputRestored {
			events = append(events, event)
		}
	}
	if len(events) != 2 || events[0].Type != eventlog.InputRestored {
		t.Fatalf("input events = %+v, want input_restored after input_lost", events)
	}
	details, ok := events[0].Details.(map[string]any)
	if !ok {
		t.Fatalf("input_restored details = %T, want map", events[0].Details)
	}
	if details["input"] != "test-device" {
		t.Fatalf("input_restored input = %v, want test-device", details["input"])
	}
	if duration, _ := details["duration_ms"].(float64); duration <= 0 {
		t.Fatalf("input_restored duration_ms = %v, want > 0", details["duration_ms"])
	}
}

func TestSourceWithoutAudioIsReopenedAndNeverRestored(t *testing.T) {
	// "sleep" keeps the capture process alive without writing any PCM.
	e := newSourceLifecycleTestEncoder(t, "sleep")
	logPath := attachEventLog(t, e)
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)
	e.inputStableDelay = time.Millisecond
	e.inputStartupTimeout = 300 * time.Millisecond

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, 10*time.Second, "stalled source reopened twice", func() bool {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.retryCount >= 2
	})
	if got := e.Status().LastError; got != "no audio from input within 300ms" {
		t.Fatalf("Status().LastError = %q, want the stall error", got)
	}
	if got := countEvents(t, e, logPath, eventlog.InputLost); got != 1 {
		t.Fatalf("input_lost events = %d, want 1", got)
	}
	if got := countEvents(t, e, logPath, eventlog.InputRestored); got != 0 {
		t.Fatalf("input_restored events = %d, want 0 for a source that never delivered audio", got)
	}
}

func TestInputOutageSpansStopAndStart(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	logPath := attachEventLog(t, e)
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)
	e.inputStableDelay = 300 * time.Millisecond
	var healthy atomic.Bool
	e.buildCaptureCommand = func(device, ffmpegPath string) (string, []string, error) {
		mode := "exit"
		if healthy.Load() {
			mode = "pcm"
		}
		return helperCaptureCommand(mode)(device, ffmpegPath)
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, 10*time.Second, "input_lost logged", func() bool {
		return countEvents(t, e, logPath, eventlog.InputLost) == 1
	})
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	// Restarted with a working input: the outage reported before the stop
	// must still be closed, or its incident and Zabbix problem stay open.
	healthy.Store(true)
	if err := e.Start(); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	waitForCondition(t, 10*time.Second, "input_restored logged after restart", func() bool {
		return countEvents(t, e, logPath, eventlog.InputRestored) == 1
	})
	if got := countEvents(t, e, logPath, eventlog.InputLost); got != 1 {
		t.Fatalf("input_lost events = %d, want 1 for the single outage", got)
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
func TestStreamStatusesAllowsUnverifiedSRT(t *testing.T) {
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
		srtAvailable:  srtUsable(false, errors.New("probe timed out")),
		streamManager: streaming.NewManager("ffmpeg"),
	}
	statuses := e.StreamStatuses([]types.Stream{stream})
	status := statuses[stream.ID]
	if status.State != types.ProcessStopped {
		t.Fatalf("status state = %q, want stopped", status.State)
	}
	if status.Error != "" {
		t.Fatalf("status error = %q, want empty", status.Error)
	}
	if !e.SRTAvailable() {
		t.Fatal("SRTAvailable() = false, want true when the probe is inconclusive")
	}
	if got := e.SRTErrorMessage(); got != "" {
		t.Fatalf("SRTErrorMessage() = %q, want empty", got)
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
func TestStartStreamAttemptsUnverifiedSRT(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Streaming.Streams = []types.Stream{
		{
			ID:      "stream-1",
			Enabled: true,
			Host:    "stream.example.com",
			Port:    freeUDPPort(t),
			Codec:   types.CodecMP3,
		},
	}
	e := &Encoder{
		config:        cfg,
		ffmpegPath:    nonexistentFFmpeg,
		state:         types.StateRunning,
		stopChan:      make(chan struct{}),
		srtAvailable:  srtUsable(false, errors.New("probe timed out")),
		streamManager: streaming.NewManager(nonexistentFFmpeg),
	}
	err := e.StartStream("stream-1")
	if err == nil {
		t.Fatal("StartStream() unexpectedly succeeded with nonexistent ffmpeg")
	}
	if errors.Is(err, ErrSRTUnsupported) {
		t.Fatalf("StartStream() error = %v, want real FFmpeg start error instead of SRT sentinel", err)
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
		{
			ID:      "caller-1",
			Enabled: true,
			Mode:    types.StreamModeCaller,
			Host:    "stream.example.com",
			Port:    freeUDPPort(t),
			Codec:   types.CodecMP3,
		},
	}
	e := &Encoder{
		config:        cfg,
		ffmpegPath:    nonexistentFFmpeg,
		state:         types.StateRunning,
		stopChan:      make(chan struct{}),
		srtAvailable:  false,
		streamManager: streaming.NewManager(nonexistentFFmpeg),
	}
	err := e.StartStream("listener-1")
	if err == nil {
		t.Fatal("StartStream() unexpectedly succeeded with nonexistent ffmpeg")
	}
	if errors.Is(err, ErrSRTUnsupported) {
		t.Fatalf("StartStream() error = %v, want real listener start error instead of SRT sentinel", err)
	}
	if err := e.StartStream("caller-1"); !errors.Is(err, ErrSRTUnsupported) {
		t.Fatalf("StartStream(caller) error = %v, want %v", err, ErrSRTUnsupported)
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
func newSourceLifecycleTestEncoder(t *testing.T, helperMode string) *Encoder {
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
		ffmpegPath:          "ffmpeg",
		buildCaptureCommand: helperCaptureCommand(helperMode),
		streamRestartDelay:  testStreamRestartDelay,
		inputStableDelay:    types.StableThreshold,
		inputStartupTimeout: inputStartupTimeout,
		inputStallTimeout:   inputStallTimeout,
		streamManager:       streaming.NewManager(""),
		recordingManager:    recordingManager,
		silenceDumpManager:  silencedump.NewManager("", 0, false, 0, nil),
		state:               types.StateStopped,
		backoff:             util.NewBackoff(types.InitialRetryDelay, types.SourceMaxRetryDelay),
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

// attachEventLog routes the encoder's alert events to a temporary event log.
func attachEventLog(t *testing.T, e *Encoder) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "encoder.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatalf("eventlog.NewLogger() error = %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	e.alertOrchestrator.SetEventLogger(logger)
	return logPath
}

func countEvents(t *testing.T, e *Encoder, logPath string, eventType eventlog.EventType) int {
	t.Helper()
	e.alertOrchestrator.DrainLogs()
	events, _, err := eventlog.ReadLast(logPath, eventlog.MaxReadLimit, 0, eventlog.FilterAll)
	if err != nil {
		t.Fatalf("ReadLast() error = %v", err)
	}
	count := 0
	for i := range events {
		if events[i].Type == eventType {
			count++
		}
	}
	return count
}

func helperCaptureCommand(mode string) func(string, string) (string, []string, error) {
	return func(_, _ string) (string, []string, error) {
		args := []string{"-test.run=TestEncoderCaptureHelperProcess", "--", mode}
		return os.Args[0], args, nil
	}
}

type fakeDanteSource struct {
	*io.PipeReader
	done <-chan struct{}
}

func (s fakeDanteSource) Wait() error {
	<-s.done
	return nil
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
		select {
		case <-signals:
		case <-time.After(time.Minute):
		}
		signal.Stop(signals)
		os.Exit(0)
	case "pcm":
		// Write 100 ms of silent S16LE stereo PCM every 100 ms until signalled.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, util.ShutdownSignals()...)
		chunk := make([]byte, audio.BytesPerSecond/10)
		deadline := time.After(time.Minute)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(0)
			}
			select {
			case <-signals:
				os.Exit(0)
			case <-deadline:
				os.Exit(0)
			case <-time.After(100 * time.Millisecond):
			}
		}
	case "burst":
		runBurstCapture()
	default:
		os.Exit(2)
	}
}

// runBurstCapture writes PCM spanning 3.2 s with a 2.5 s pause, then stays
// alive without writing until it is signalled.
func runBurstCapture() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, util.ShutdownSignals()...)
	chunk := make([]byte, audio.BytesPerSecond/10)
	for _, pause := range []time.Duration{100, 100, 2500, 100, 100, 100, 100, 100, 100} {
		if _, err := os.Stdout.Write(chunk); err != nil {
			os.Exit(0)
		}
		time.Sleep(pause * time.Millisecond)
	}
	select {
	case <-signals:
	case <-time.After(time.Minute):
	}
	os.Exit(0)
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
func assertManagersStopped(t *testing.T, e *Encoder, reason string) {
	t.Helper()
	if e.recordingManager.IsRunning() {
		t.Fatalf("recording manager is running: %s", reason)
	}
	if e.silenceDumpManager.IsRunning() {
		t.Fatalf("silence dump manager is running: %s", reason)
	}
}

func TestSourceThatStopsDeliveringIsReopenedAndNeverRestored(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	logPath := attachEventLog(t, e)
	e.backoff = util.NewBackoff(time.Millisecond, 2*time.Millisecond)
	e.inputStableDelay = 3 * time.Second
	e.inputStallTimeout = 3 * time.Second
	var attempts atomic.Int32
	e.buildCaptureCommand = func(device, ffmpegPath string) (string, []string, error) {
		mode := "burst"
		if attempts.Add(1) == 1 {
			mode = "exit"
		}
		return helperCaptureCommand(mode)(device, ffmpegPath)
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, 15*time.Second, "stalled source reopened", func() bool {
		return e.Status().LastError == "no audio from input for 3s"
	})
	if got := countEvents(t, e, logPath, eventlog.InputRestored); got != 0 {
		t.Fatalf("input_restored events = %d, want 0 for bursts that never stay steady", got)
	}
}
