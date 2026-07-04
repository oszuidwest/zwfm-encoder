//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"path/filepath"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/tray"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// runShell installs the Windows systray and blocks until either a shutdown
// signal or the tray Quit item cancels the context.
func runShell(ctx context.Context, cancel context.CancelFunc, a *app) {
	sigCtx, stop := signal.NotifyContext(ctx, util.ShutdownSignals()...)
	defer stop()

	cfg := a.cfg.Snapshot()
	logDir := filepath.Dir(eventlog.DefaultLogPath(cfg.WebPort))

	trayCfg := tray.Config{
		AppName: cfg.StationName,
		URL:     fmt.Sprintf("http://localhost:%d", cfg.WebPort),
		LogDir:  logDir,
		StartEncoder: func() error {
			if !a.ffmpegAvailable {
				return tray.ErrFFmpegMissing
			}
			return a.encoder.Start()
		},
		StopEncoder: a.encoder.Stop,
		Status: func() tray.Status {
			if !a.ffmpegAvailable {
				return tray.StatusFFmpegMissing
			}
			if a.encoder.IsRunning() {
				return tray.StatusRunning
			}
			return tray.StatusStopped
		},
	}

	// Bridge signal cancellation into a tray Quit so systray unblocks on
	// SIGTERM / service stop as well as menu clicks.
	go func() {
		<-sigCtx.Done()
		tray.Quit()
	}()

	tray.Run(trayCfg, func() {
		slog.Info("tray exit requested")
		cancel()
	})
}
