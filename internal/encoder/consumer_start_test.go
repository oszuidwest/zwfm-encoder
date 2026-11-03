package encoder

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/streaming"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestStopCancelsDelayedProductionConsumerStarts(t *testing.T) {
	for _, tc := range []struct {
		name, message    string
		manual, listener bool
	}{
		{name: "manual reserved stream", message: "starting stream", manual: true},
		{name: "automatic reserved stream", message: "starting stream"},
		{name: "automatic before reservation", message: "skipping disabled stream"},
		{name: "manual reserved listener", message: "starting listener encoder", manual: true, listener: true},
		{name: "automatic reserved listener", message: "starting listener encoder", listener: true},
		{name: "automatic dump manager", message: "audio incident dump manager started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSourceLifecycleTestEncoder(t, "exit")
			t.Cleanup(e.alertOrchestrator.Close)
			e.state, e.sourceRunID = types.StateRunning, 1
			e.stopChan, e.sourceStdout = make(chan struct{}), strings.NewReader("")
			e.srtAvailable = true
			e.streamManager = streaming.NewManager(nonexistentFFmpeg)
			e.config.Streaming.Streams = []types.Stream{
				{ID: "disabled", Host: "localhost", Port: 9000, Codec: types.CodecMP3},
				{ID: "enabled", Enabled: true, Host: "localhost", Port: 9001, Codec: types.CodecMP3},
			}
			if tc.listener {
				e.config.Streaming.Streams[1].Mode = types.StreamModeListener
				e.config.Streaming.Streams[1].Host = "127.0.0.1"
				e.config.Streaming.Streams[1].Port = freeUDPPort(t)
			}
			canceled := make(chan struct{})
			e.sourceCancel = func() { close(canceled) }
			previous := slog.Default()
			previousOutput, previousFlags := log.Writer(), log.Flags()
			entered, blocked, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			slog.SetDefault(slog.New(&delayedStartLog{Handler: slog.NewTextHandler(io.Discard, nil), message: tc.message, entered: entered, blocked: blocked}))
			go func() {
				defer close(started)
				if tc.manual {
					_ = e.StartStream("enabled")
				} else {
					e.startEnabledStreams(1)
				}
			}()
			t.Cleanup(func() {
				release()
				<-started
				slog.SetDefault(previous)
				log.SetOutput(previousOutput)
				log.SetFlags(previousFlags)
			})
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("production startup did not reach the delayed operation")
			}
			stopped := make(chan error, 1)
			go func() { stopped <- e.Stop() }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(types.ShutdownTimeout):
				release()
				<-stopped
				t.Fatal("Stop waited for a delayed consumer operation instead of canceling it")
			}
			select {
			case <-canceled:
			default:
				t.Fatal("consumer startup prevented source cancellation")
			}
			if tc.listener {
				port := e.config.Streaming.Streams[1].Port
				conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Fatalf("Stop left the reserved listener port open: %v", err)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			}
			release()
			<-started
			assertManagersStopped(t, e, "delayed production startup escaped Stop")
		})
	}
}

func TestConsumerStartAllowed(t *testing.T) {
	t.Parallel()
	current, previous := make(chan struct{}), make(chan struct{})
	for _, tc := range []struct {
		name     string
		state    types.EncoderState
		stdout   io.Reader
		runID    uint64
		stopChan chan struct{}
		want     bool
	}{
		{"manual in current run", types.StateRunning, nil, 0, current, true},
		{"manual captured before a restart", types.StateRunning, nil, 0, previous, false},
		{"automatic for current run", types.StateRunning, strings.NewReader(""), 2, current, true},
		{"automatic for ended run", types.StateRunning, strings.NewReader(""), 1, current, false},
		{"automatic without stdout", types.StateRunning, nil, 2, current, false},
		{"stopping", types.StateStopping, strings.NewReader(""), 0, current, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Encoder{state: tc.state, stopChan: current, sourceRunID: 2, sourceStdout: tc.stdout}
			if got := e.consumerStartAllowed(tc.runID, tc.stopChan); got != tc.want {
				t.Fatalf("consumerStartAllowed = %t, want %t", got, tc.want)
			}
		})
	}
}

// A consumer stop that blocks must not keep capture running.
func TestStopCancelsSourceBeforeStoppingConsumers(t *testing.T) {
	e := newSourceLifecycleTestEncoder(t, "exit")
	t.Cleanup(e.alertOrchestrator.Close)
	e.state, e.sourceRunID = types.StateRunning, 1
	e.stopChan, e.sourceStdout = make(chan struct{}), strings.NewReader("")
	if err := e.recordingManager.Start(); err != nil {
		t.Fatal(err)
	}
	canceled := make(chan struct{})
	e.sourceCancel = func() { close(canceled) }
	previous := slog.Default()
	previousOutput, previousFlags := log.Writer(), log.Flags()
	entered, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	slog.SetDefault(slog.New(&delayedStartLog{Handler: slog.NewTextHandler(io.Discard, nil), message: "recording manager stopped", entered: entered, blocked: blocked}))
	stopped := make(chan error, 1)
	t.Cleanup(func() {
		release()
		<-stopped
		slog.SetDefault(previous)
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})
	go func() { stopped <- e.Stop() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not reach the recording manager stop")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("source kept running while consumers were being stopped")
	}
}

type delayedStartLog struct {
	slog.Handler
	message          string
	entered, blocked chan struct{}
	once             sync.Once
}

//nolint:gocritic // slog.Handler requires the record to be passed by value.
func (h *delayedStartLog) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.message {
		h.once.Do(func() { close(h.entered); <-h.blocked })
	}
	return h.Handler.Handle(ctx, record)
}

// A saved stream must not keep its old process: it restarts with the saved
// settings, or with the next source run while the input is being reopened.
func TestSavedStreamFollowsSettings(t *testing.T) {
	// Keep this test serial: one case swaps the package-wide default logger.
	if runtime.GOOS == "windows" {
		t.Skip("the FFmpeg stand-in is a shell script")
	}
	for _, tc := range []struct {
		name                            string
		enabled, outage, backDuringStop bool
	}{
		{name: "disabled during outage", outage: true},
		{name: "disabled as the input returns", outage: true, backDuringStop: true},
		{name: "edited during outage", enabled: true, outage: true},
		{name: "edited while running", enabled: true},
		{name: "edited as the input returns", enabled: true, outage: true, backDuringStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSourceLifecycleTestEncoder(t, "exit")
			t.Cleanup(e.alertOrchestrator.Close)
			// Each start appends its PID and arguments to ffmpeg.args.
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\necho \"$$ $@\" >> \"$0.args\"\nexec sleep 60\n"), 0o700); err != nil { //nolint:gosec // Executable test stand-in.
				t.Fatal(err)
			}
			e.streamManager = streaming.NewManager(ffmpeg)
			e.srtAvailable = true
			stream := types.Stream{ID: "s1", Enabled: true, Host: "localhost", Port: 9000, Codec: types.CodecPCM}
			unsaved := types.Stream{ID: "s2", Enabled: true, Host: "localhost", Port: 9100, Codec: types.CodecPCM}
			e.config.Streaming.Streams = []types.Stream{stream, unsaved}

			// Run 1 is live and both streams run.
			e.mu.Lock()
			e.state, e.sourceRunID = types.StateRunning, 1
			e.stopChan, e.sourceStdout = make(chan struct{}), strings.NewReader("")
			e.mu.Unlock()
			for _, id := range []string{"s1", "s2"} {
				if err := e.StartStream(id); err != nil {
					t.Fatal(err)
				}
			}
			savedProcess := fakeFFmpegProcess(t, ffmpeg, ":9000?")
			unsavedProcess := fakeFFmpegProcess(t, ffmpeg, ":9100?")

			if tc.outage {
				// Run 1 failed; runSourceLoop backs off in StateStarting.
				e.mu.Lock()
				e.state, e.sourceStdout = types.StateStarting, nil
				e.mu.Unlock()
			}

			// The operator saves the stream, as handleUpdateStream does.
			stream.Enabled, stream.Port = tc.enabled, 9001
			if err := e.config.UpdateStream(&stream); err != nil {
				t.Fatal(err)
			}
			publishRun2 := func() {
				e.mu.Lock()
				e.state, e.sourceRunID, e.sourceStdout = types.StateRunning, 2, strings.NewReader("")
				e.mu.Unlock()
			}
			if tc.backDuringStop {
				// Run 2 publishes after the state check, and its starter has
				// skipped the stream that still ran: it will not start it.
				restartWhileRunPublishes(t, e, "s1", publishRun2)
				assertExited(t, savedProcess)
				if !tc.enabled {
					// The delayed restart in run 2 must not start a disabled stream.
					time.Sleep(types.StreamRestartDelay + 100*time.Millisecond)
				}
			} else {
				e.RestartStream("s1")
				assertExited(t, savedProcess)
				if tc.outage && tc.enabled {
					// The delayed restart must not start the stream while the input is down.
					time.Sleep(types.StreamRestartDelay + 100*time.Millisecond)
					if e.streamManager.Statuses(e.config.Stream)["s1"].State == types.ProcessRunning {
						t.Fatal("stream has a running process during the outage")
					}
				}
				if tc.outage {
					// Run 2 publishes and its delayed starter runs.
					publishRun2()
					e.startEnabledStreams(2)
				}
			}

			if !tc.enabled {
				// StreamStatuses would report any process of a disabled stream as disabled.
				if status, exists := e.streamManager.Statuses(e.config.Stream)["s1"]; exists {
					t.Fatalf("disabled stream still has a process in state %q", status.State)
				}
			} else if err := fakeFFmpegProcess(t, ffmpeg, ":9001?").Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the edited stream lost its FFmpeg process: %v", err)
			}
			if err := unsavedProcess.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the unsaved stream lost its FFmpeg process: %v", err)
			}
		})
	}
}

// restartWhileRunPublishes calls RestartStream and runs publish while the
// stop is in progress, after RestartStream checked the encoder state.
func restartWhileRunPublishes(t *testing.T, e *Encoder, streamID string, publish func()) {
	t.Helper()
	previous := slog.Default()
	previousOutput, previousFlags := log.Writer(), log.Flags()
	entered, blocked, restarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	slog.SetDefault(slog.New(&delayedStartLog{Handler: slog.NewTextHandler(io.Discard, nil), message: "stopping stream", entered: entered, blocked: blocked}))
	go func() {
		defer close(restarted)
		e.RestartStream(streamID)
	}()
	t.Cleanup(func() {
		release()
		<-restarted
		slog.SetDefault(previous)
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("RestartStream did not stop the stream")
	}
	publish()
	release()
	<-restarted
}

// assertExited fails unless the saved stream's old process is gone; Stop waits
// for it, so it must be gone once RestartStream returns.
func assertExited(t *testing.T, process *os.Process) {
	t.Helper()
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("the saved stream's old FFmpeg process still answers signal 0")
	}
}

// fakeFFmpegProcess waits until the FFmpeg stand-in at path logs a start with
// marker in its arguments, and returns the process of the latest such start.
func fakeFFmpegProcess(t *testing.T, path, marker string) *os.Process {
	t.Helper()
	deadline := time.Now().Add(types.StreamRestartDelay + 3*time.Second)
	for {
		args, _ := os.ReadFile(path + ".args") //nolint:gosec // Test temp dir.
		pid := 0
		for line := range strings.Lines(string(args)) {
			if strings.Contains(line, marker) {
				pid, _ = strconv.Atoi(strings.Fields(line)[0])
			}
		}
		if pid != 0 {
			process, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			return process
		}
		if time.Now().After(deadline) {
			t.Fatalf("FFmpeg starts = %q, want one with %s", args, marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
