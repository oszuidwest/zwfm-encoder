package streaming

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
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
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("resumed startup = %v, want cancellation before process creation", err)
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
