//go:build windows

package tray

import (
	"log/slog"
	"os/exec"
	"time"

	"fyne.io/systray"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Run installs the tray icon and menu, then blocks until the user picks Quit
// or Quit is invoked externally. onExit fires after the tray tears down.
// Must be called on the main OS thread — the systray library requires it on
// Windows.
func Run(cfg Config, onExit func()) {
	onReady := func() {
		installMenu(cfg)
	}
	systray.Run(onReady, onExit)
}

// Quit signals the tray to exit; safe to call from any goroutine.
func Quit() {
	systray.Quit()
}

func installMenu(cfg Config) {
	systray.SetIcon(iconFor(cfg.Status()))
	systray.SetTitle(cfg.AppName)
	systray.SetTooltip(cfg.AppName)

	openUI := systray.AddMenuItem("Open UI", "Open the web interface in the default browser")
	systray.AddSeparator()
	startItem := systray.AddMenuItem("Start Encoder", "Start audio capture and streaming")
	stopItem := systray.AddMenuItem("Stop Encoder", "Stop audio capture and streaming")
	systray.AddSeparator()
	showLogs := systray.AddMenuItem("Show Logs Folder", "Open the log folder in Explorer")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Exit the encoder")

	go handleClicks(cfg, openUI, startItem, stopItem, showLogs, quit)
	go pollStatus(cfg, startItem, stopItem)
}

func handleClicks(cfg Config, openUI, startItem, stopItem, showLogs, quit *systray.MenuItem) {
	for {
		select {
		case <-openUI.ClickedCh:
			if err := openBrowser(cfg.URL); err != nil {
				slog.Error("tray: open browser failed", "error", err, "url", cfg.URL)
			}
		case <-startItem.ClickedCh:
			if err := cfg.StartEncoder(); err != nil {
				slog.Error("tray: start encoder failed", "error", err)
			}
		case <-stopItem.ClickedCh:
			if err := cfg.StopEncoder(); err != nil {
				slog.Error("tray: stop encoder failed", "error", err)
			}
		case <-showLogs.ClickedCh:
			if err := openFolder(cfg.LogDir); err != nil {
				slog.Error("tray: open logs folder failed", "error", err, "dir", cfg.LogDir)
			}
		case <-quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// pollStatus grays out Start / Stop based on the current encoder state, keeps
// the tooltip in sync, and swaps the tray icon whenever the status changes
// (green = running, amber = stopped, red = FFmpeg missing). Polling is fine
// here — status changes are rare and the tray is not on any hot path.
func pollStatus(cfg Config, startItem, stopItem *systray.MenuItem) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var last Status = -1

	apply := func() {
		s := cfg.Status()
		switch s {
		case StatusRunning:
			startItem.Disable()
			stopItem.Enable()
			systray.SetTooltip(cfg.AppName + " — running")
		case StatusStopped:
			startItem.Enable()
			stopItem.Disable()
			systray.SetTooltip(cfg.AppName + " — stopped")
		case StatusFFmpegMissing:
			startItem.Disable()
			stopItem.Disable()
			systray.SetTooltip(cfg.AppName + " — FFmpeg missing")
		}
		if s != last {
			systray.SetIcon(iconFor(s))
			last = s
		}
	}

	apply()
	for range ticker.C {
		apply()
	}
}

// openBrowser launches the default browser via rundll32.
func openBrowser(url string) error {
	//nolint:gosec // URL is our own http://localhost:<port>, not user input
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	util.HideConsole(cmd)
	return cmd.Start()
}

// openFolder opens the given directory in Explorer.
func openFolder(dir string) error {
	//nolint:gosec // dir is the fixed event-log directory, not user input
	cmd := exec.Command("explorer.exe", dir)
	util.HideConsole(cmd)
	return cmd.Start()
}
