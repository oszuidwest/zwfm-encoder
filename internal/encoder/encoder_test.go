package encoder

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/notify"
	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

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
