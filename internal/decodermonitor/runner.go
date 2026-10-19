package decodermonitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/notify"
)

// Sender is the existing Graph client's SendMail contract.
type Sender interface {
	SendMail(context.Context, []string, string, string) error
}

// Runner has an independent mail worker so M365 cannot block local observation.
type Runner struct {
	Config     Config
	Store      *Store
	Probe      Prober
	Sender     Sender
	Recipients []string
}

// Run owns and joins its worker. Errors stop only the watchdog, never MicroMPX.
func (r *Runner) Run(ctx context.Context) error {
	if r.Store == nil || r.Probe == nil || (r.Config.SendMail && r.Sender == nil) {
		return fmt.Errorf("watchdog requires a store, probe and configured mail sender")
	}
	if err := r.Store.Update(nil, time.Now(), func(s *State) error {
		// A restarted watchdog cannot count unobserved downtime toward recovery.
		s.LastObserved, s.Incident.CleanSince = time.Time{}, time.Time{}
		s.ObservationOK, s.Stalled = false, 0
		return nil
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workers.Go(func() { r.mailLoop(ctx) })
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(seconds(r.Config.PollSeconds))
	defer ticker.Stop()
	heartbeat := time.Now().Add(-time.Minute)
	pruned := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := r.poll(ctx); err != nil {
			return err
		}
		now := time.Now()
		if now.Sub(pruned) >= time.Hour {
			if err := r.Store.Prune(now); err != nil {
				return err
			}
			pruned = now
		}
		if r.Config.HeartbeatURL != "" && now.Sub(heartbeat) >= time.Minute {
			if r.ready(now) {
				r.ping(ctx)
			}
			heartbeat = now
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Runner) poll(ctx context.Context) error {
	state, err := r.Store.Snapshot()
	if err != nil {
		return err
	}
	probe, probeErr := r.Probe.Read(ctx, state.Bookmark)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	engine := Engine{Config: r.Config}
	if probeErr != nil {
		if state.ObservationOK || state.UnknownSince.IsZero() {
			slog.Warn("watchdog decoder observation unavailable", "error", probeErr)
		}
		return r.Store.Update(nil, now, func(s *State) error { return engine.Failed(s, now) })
	}
	if probe.JournalError && (state.ObservationOK || state.UnknownSince.IsZero()) {
		slog.Warn("watchdog journal unavailable; kernel playback checks remain active")
	}
	if !probe.JournalError && probe.Continuity == "gap" {
		slog.Warn("watchdog journal continuity lost; missing history cannot be reconstructed", "caught_up", probe.CaughtUp)
	}
	return r.Store.Update(probe.Records, now, func(s *State) error {
		return engine.Step(s, &probe, now)
	})
}

func (r *Runner) mailLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.deliver(ctx, time.Now()); err != nil {
			slog.Error("watchdog mail state update failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) deliver(ctx context.Context, now time.Time) error {
	s, err := r.Store.Snapshot()
	if err != nil {
		return err
	}
	if len(s.Outbox) == 0 || now.Before(s.Outbox[0].NextAttempt) {
		return nil
	}
	mail := s.Outbox[0]
	var sendErr error
	switch {
	case mail.Preview:
		slog.Info("watchdog notification preview (not sent)", "notification_id", mail.ID, "subject", mail.Subject)
	case !r.Config.SendMail:
		// Turning delivery off must not discard real, previously queued mail.
		return nil
	default:
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		subject := mail.Subject
		body := mail.Body
		if now.Sub(mail.Created) > time.Minute {
			subject = "[DELAYED] " + subject
			body = "This queued notification describes the state at its detection time, not necessarily the current state.\n\n" + body
		}
		sendErr = r.Sender.SendMail(ctx, r.Recipients, subject, body)
		if sendErr != nil {
			// Graph/OAuth error responses may contain identities or credentials.
			slog.Warn("watchdog M365 delivery deferred", "notification_id", mail.ID, "attempt", mail.Attempts+1)
		} else {
			slog.Info("watchdog mail accepted by M365 (not delivery confirmation)", "notification_id", mail.ID)
		}
	}
	return r.Store.Update(nil, now, func(s *State) error {
		if len(s.Outbox) == 0 || s.Outbox[0].ID != mail.ID {
			return fmt.Errorf("watchdog mail queue changed unexpectedly")
		}
		if sendErr == nil {
			s.Outbox = s.Outbox[1:]
			return nil
		}
		s.Outbox[0].Attempts++
		backoff := min(300, 1<<min(s.Outbox[0].Attempts, 8))
		s.Outbox[0].NextAttempt = now.Add(seconds(backoff))
		return nil
	})
}

func (r *Runner) ready(now time.Time) bool {
	s, err := r.Store.Snapshot()
	if err != nil || !s.ObservationOK || s.LastObserved.IsZero() || now.Sub(s.LastObserved) > 2*seconds(r.Config.PollSeconds) {
		return false
	}
	// Known decoder faults do not masquerade as a dead monitoring pipeline.
	return len(s.Outbox) == 0 || now.Sub(s.Outbox[0].Created) < 5*time.Minute
}

func (r *Runner) ping(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Config.HeartbeatURL, http.NoBody)
	if err != nil {
		slog.Warn("watchdog heartbeat request rejected")
		return
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	//nolint:gosec // Administrator-configured HTTPS heartbeat; URL is never logged.
	response, err := client.Do(req)
	if err != nil {
		slog.Warn("watchdog heartbeat failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		slog.Warn("watchdog heartbeat rejected", "status", response.StatusCode)
	}
}

// NewMailSender reuses the encoder's Graph transport without starting an encoder.
func NewMailSender(cfg *Config) (Sender, []string, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("watchdog configuration is required")
	}
	if !cfg.SendMail {
		return nil, nil, nil
	}
	email, err := LoadEmail(cfg.EmailFile)
	if err != nil {
		return nil, nil, err
	}
	client, err := notify.NewGraphClient(&email)
	if err != nil {
		return nil, nil, err
	}
	recipients := notify.ParseRecipients(email.Recipients)
	if len(recipients) == 0 {
		return nil, nil, fmt.Errorf("M365 recipient list is empty")
	}
	for _, recipient := range recipients {
		if strings.ContainsAny(recipient, "\r\n") {
			return nil, nil, fmt.Errorf("M365 recipient contains control characters")
		}
	}
	return client, recipients, nil
}
