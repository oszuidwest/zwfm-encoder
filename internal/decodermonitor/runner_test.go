package decodermonitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type fakeSender struct {
	err     error
	calls   int
	subject string
}

func (f *fakeSender) SendMail(_ context.Context, _ []string, subject, _ string) error {
	f.calls++
	f.subject = subject
	return f.err
}

func TestMailFailureRetryAndRestart(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		s.Outbox = []Mail{{ID: 1, Subject: "queued", Created: at.Add(-time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{err: errors.New("sensitive OAuth response must not be persisted")}
	r := Runner{Config: cfg, Store: store, Sender: sender, Recipients: []string{"recipient@example.invalid"}}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].Attempts != 1 || !s.Outbox[0].NextAttempt.After(at) {
		t.Fatalf("failed mail not retained: %+v %v", s, err)
	}
	if !strings.HasPrefix(sender.subject, "[DELAYED]") {
		t.Fatal("old queued mail represented as current")
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatal("retry ignored backoff")
	}
	sender.err = nil
	if err := r.deliver(t.Context(), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err = store.Snapshot()
	if err != nil || len(s.Outbox) != 0 || sender.calls != 2 {
		t.Fatalf("accepted mail not acknowledged: %+v %v", s, err)
	}
}

func TestPreviewNeverSendsAndDisabledDeliveryPreservesQueue(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	sender := &fakeSender{}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = []Mail{{ID: 1, Preview: true}, {ID: 2}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || sender.calls != 0 || len(s.Outbox) != 1 || s.Outbox[0].ID != 2 {
		t.Fatalf("preview sent or real queued mail discarded: %+v %v", s, err)
	}
}

type canceledProbe struct{ entered chan struct{} }

func (p canceledProbe) Read(ctx context.Context, _ Bookmark) (Probe, error) {
	close(p.entered)
	<-ctx.Done()
	return Probe{}, ctx.Err()
}

func TestRunnerJoinsWorkersOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		entered := make(chan struct{})
		r := Runner{Config: cfg, Store: store, Probe: canceledProbe{entered}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		synctest.Wait()
	})
}
