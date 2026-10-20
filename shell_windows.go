//go:build windows

package main

import (
	"context"
	"net"
	"net/netip"
	"strconv"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/tray"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// runShell installs the Windows systray and blocks until the tray Quit item
// (or a shutdown signal bridged into a tray Quit) exits the tray loop.
func runShell(ctx context.Context, a *app) {
	cfg := a.cfg.Snapshot()
	logDir := eventlog.DefaultLogDir(cfg.WebPort)
	// A specific bind address is not reachable through localhost.
	host := "localhost"
	if addr, err := netip.ParseAddr(cfg.WebBindAddress); err == nil && !addr.IsUnspecified() {
		host = addr.String()
	}

	trayCfg := tray.Config{
		AppName:      cfg.StationName,
		URL:          "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.WebPort)),
		LogDir:       logDir,
		StartEncoder: a.encoder.Start,
		StopEncoder:  a.encoder.Stop,
		Status: func() tray.Status {
			if !a.encoder.FFmpegAvailable() {
				return tray.StatusFFmpegMissing
			}
			if a.encoder.State() == types.StateStopped {
				return tray.StatusStopped
			}
			return tray.StatusRunning // running, starting, or stopping
		},
	}

	// Bridge ctx cancellation (Ctrl+C when a console is attached) into a
	// tray Quit so systray unblocks on interrupt as well as menu clicks.
	// Windows delivers no SIGTERM, and a GUI build without console receives
	// no interrupt at all - there the tray menu is the only exit path.
	go func() {
		<-ctx.Done()
		tray.Quit()
	}()

	tray.Run(trayCfg)
}
