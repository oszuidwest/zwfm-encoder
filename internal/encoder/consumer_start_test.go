package encoder

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net"
	"strings"
	"sync"
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
		{name: "automatic recorder manager", message: "recording manager started"},
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
