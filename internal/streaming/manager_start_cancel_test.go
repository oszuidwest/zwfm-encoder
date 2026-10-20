package streaming

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestStopCancelsStartWaitingForOldStreamResources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager("/nonexistent/ffmpeg-test")
		cfg := validStream()
		old := &Stream{state: types.ProcessError, audioCh: make(chan []byte)}
		old.writerWg.Add(1)
		m.streams[cfg.ID] = old
		done := make(chan error, 1)
		go func() { _, err := m.Start(cfg); done <- err }()
		synctest.Wait()
		if err := m.Stop(cfg.ID); err != nil {
			old.writerWg.Done()
			t.Fatal(err)
		}
		old.writerWg.Done()
		if err := <-done; err != nil {
			t.Fatalf("resumed startup = %v, want a cancelled claim reported like a lost one", err)
		}
		if len(m.Statuses(func(string) *types.Stream { return cfg })) != 0 {
			t.Fatal("canceled startup published a late stream")
		}
	})
}

func TestStartIfActiveRejectsEndedSource(t *testing.T) {
	m := NewManager("/nonexistent/ffmpeg-test")
	cfg := validStream()
	started, err := m.StartIfActive(cfg, func() bool { return false })
	statuses := m.Statuses(func(string) *types.Stream { return cfg })
	if err != nil || started || len(statuses) != 0 {
		t.Fatalf("ended source started a stream: started=%t, err=%v, status=%v", started, err, statuses)
	}
}

func TestStopCancelsReservedListenerEncoderRetry(t *testing.T) {
	m := NewManager("/nonexistent/ffmpeg-test")
	cfg := validStream()
	cfg.Mode = types.StreamModeListener
	stream := &Stream{mode: types.StreamModeListener, state: types.ProcessRunning}
	m.streams[cfg.ID] = stream
	ctx, cancel := m.reserveListenerEncoderStart(cfg.ID, stream, func() bool { return true })
	if ctx == nil {
		t.Fatal("active listener retry could not reserve startup")
	}
	defer cancel(nil)
	if err := m.Stop(cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.startListenerEncoderRun(ctx, cfg.ID, stream, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("resumed listener retry = %v, want cancellation before exec", err)
	}
	if next, _ := m.reserveListenerEncoderStart(cfg.ID, stream, func() bool { return true }); next != nil {
		t.Fatal("stopped listener reserved another retry")
	}
}

func TestListenerRetryLosingItsSourceDoesNotStrandFanout(t *testing.T) {
	m := NewManager(fakeLongRunningExecutable(t))
	cfg := validStream()
	cfg.Mode, cfg.Host, cfg.Port = types.StreamModeListener, "127.0.0.1", freeUDPPort(t)
	if started, err := m.Start(cfg); err != nil || !started {
		t.Fatalf("listener start = %t, %v", started, err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); _ = m.Stop(cfg.ID); <-done })
	entry := m.current(cfg.ID)
	entry.backoff = util.NewBackoff(time.Millisecond, time.Millisecond)
	ctx := &sourceEndingOnRetryConfig{cfg: cfg}
	ctx.running.Store(true)
	go func() { defer close(done); m.MonitorAndRetry(cfg.ID, ctx, stop) }()
	run := waitListenerRun(t, m, cfg.ID, nil)
	if err := run.result.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("listener monitor did not finish after losing its source")
	}
	if m.current(cfg.ID) != nil {
		t.Fatal("source loss left a running listener entry with no encoder to retry")
	}
	assertUDPPortAvailable(t, cfg.Port)
}

func TestCallerRetryLosingSourceRemovesItsEntry(t *testing.T) {
	m := NewManager(fakeLongRunningExecutable(t))
	cfg := validStream()
	if ok, err := m.Start(cfg); err != nil || !ok {
		t.Fatalf("Start() = %v, %v", ok, err)
	}
	entry := m.current(cfg.ID)
	entry.backoff = util.NewBackoff(time.Millisecond, time.Millisecond)
	ctx := &sourceEndingOnRetryConfig{cfg: cfg}
	ctx.running.Store(true)
	stop, done := make(chan struct{}), make(chan struct{})
	defer close(stop)
	go func() { defer close(done); m.MonitorAndRetry(cfg.ID, ctx, stop) }()
	if err := entry.result.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("caller monitor did not finish after losing its source")
	}
	if m.current(cfg.ID) != nil {
		t.Fatal("a rejected retry left the failed entry and its blocked writer behind")
	}
	entry.writerWg.Wait()
}

type sourceEndingOnRetryConfig struct {
	cfg     *types.Stream
	reads   atomic.Int32
	running atomic.Bool
}

func (c *sourceEndingOnRetryConfig) IsRunning() bool { return c.running.Load() }

func (c *sourceEndingOnRetryConfig) Stream(string) *types.Stream {
	if c.reads.Add(1) == 2 {
		c.running.Store(false) // The source exits while the post-backoff config is being read.
	}
	return c.cfg
}

func TestStartPredicatesRunUnderStopLock(t *testing.T) {
	m := NewManager("/nonexistent/ffmpeg-test")
	cfg := validStream()
	// Checked under the lock Stop takes, or a start can pass it just before Stop.
	underStopLock := func() bool {
		if m.mu.TryLock() {
			m.mu.Unlock()
			t.Error("start predicate ran without the lock Stop takes")
		}
		return false
	}
	_, _ = m.StartIfActive(cfg, underStopLock)
	stream := &Stream{mode: types.StreamModeListener, state: types.ProcessRunning}
	m.streams[cfg.ID] = stream
	m.reserveListenerEncoderStart(cfg.ID, stream, underStopLock)
}

func TestStopKillsProcessOfStreamMarkedErrorByWriter(t *testing.T) {
	// The fake closes stdin, so writes fail while the process keeps running.
	m := NewManager(writeFakeFFmpeg(t, "#!/bin/sh\nexec 0<&-\nexec sleep 3600\n"))
	cfg := validStream()
	if started, err := m.Start(cfg); err != nil || !started {
		t.Fatalf("Start() = %t, %v", started, err)
	}
	result := m.current(cfg.ID).result
	t.Cleanup(func() { result.Cancel(nil); _ = result.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.WriteAudioFanOut(make([]byte, 4096))
		m.mu.RLock()
		state := m.streams[cfg.ID].state
		m.mu.RUnlock()
		if state == types.ProcessError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never marked the stream as errored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Stop(cfg.ID); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = result.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop left the errored stream's FFmpeg process running")
	}
}
