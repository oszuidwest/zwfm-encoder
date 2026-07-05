//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"path/filepath"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/tray"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// runShell installs the Windows systray and blocks until the tray Quit item
// (or an os.Interrupt bridged into a tray Quit) exits the tray loop.
func runShell(a *app) {
	sigCtx, stop := signal.NotifyContext(context.Background(), util.ShutdownSignals()...)
	defer stop()

	cfg := a.cfg.Snapshot()
	logDir := filepath.Dir(eventlog.DefaultLogPath(cfg.WebPort))

	trayCfg := tray.Config{
		AppName: cfg.StationName,
		URL:     fmt.Sprintf("http://localhost:%d", cfg.WebPort),
		LogDir:  logDir,
		StartEncoder: func() error {
			// Guard here: encoder.Start does not check FFmpeg availability
			// and would enter its retry loop on a doomed start.
			if !a.server.ffmpegAvailable {
				return errors.New("ffmpeg not available")
			}
			return a.encoder.Start()
		},
		StopEncoder: a.encoder.Stop,
		Status: func() tray.Status {
			if !a.server.ffmpegAvailable {
				return tray.StatusFFmpegMissing
			}
			if a.encoder.IsRunning() {
				return tray.StatusRunning
			}
			return tray.StatusStopped
		},
	}

	// Bridge os.Interrupt (Ctrl+C when a console is attached) into a tray
	// Quit so systray unblocks on interrupt as well as menu clicks. Windows
	// delivers no SIGTERM, and a GUI build without console receives no
	// interrupt at all - there the tray menu is the only exit path.
	go func() {
		<-sigCtx.Done()
		tray.Quit()
	}()

	tray.Run(trayCfg, func() {
		slog.Info("tray exit requested")
	})
}
