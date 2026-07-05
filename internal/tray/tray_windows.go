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
// Must be called from the main goroutine: fyne.io/systray locks that
// goroutine to its OS thread at package init and runs the message loop there.
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
	systray.SetTitle(cfg.AppName)

	openUI := systray.AddMenuItem("Open UI", "Open the web interface in the default browser")
	systray.AddSeparator()
	startItem := systray.AddMenuItem("Start Encoder", "Start audio capture and streaming")
	stopItem := systray.AddMenuItem("Stop Encoder", "Stop audio capture and streaming")
	systray.AddSeparator()
	showLogs := systray.AddMenuItem("Show Logs Folder", "Open the log folder in Explorer")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Exit the encoder")

	status := cfg.Status()
	applyStatus(cfg, status, startItem, stopItem)

	go handleClicks(cfg, openUI, startItem, stopItem, showLogs, quit)
	go pollStatus(cfg, status, startItem, stopItem)
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
				systray.SetTooltip(cfg.AppName + " - start failed: " + err.Error())
			}
		case <-stopItem.ClickedCh:
			if err := cfg.StopEncoder(); err != nil {
				slog.Error("tray: stop encoder failed", "error", err)
				systray.SetTooltip(cfg.AppName + " - stop failed: " + err.Error())
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

// pollStatus watches the encoder state and reapplies the menu, tooltip, and
// icon whenever it changes. Polling is fine here - status changes are rare
// and the tray is not on any hot path. Applying only on change also lets an
// error tooltip set by handleClicks persist until the status actually moves.
func pollStatus(cfg Config, last Status, startItem, stopItem *systray.MenuItem) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if s := cfg.Status(); s != last {
			applyStatus(cfg, s, startItem, stopItem)
			last = s
		}
	}
}

// applyStatus grays out Start / Stop, updates the tooltip, and swaps the
// tray icon for the given status (green = running, amber = stopped,
// red = FFmpeg missing).
func applyStatus(cfg Config, s Status, startItem, stopItem *systray.MenuItem) {
	switch s {
	case StatusRunning:
		startItem.Disable()
		stopItem.Enable()
		systray.SetTooltip(cfg.AppName + " - running")
	case StatusStopped:
		startItem.Enable()
		stopItem.Disable()
		systray.SetTooltip(cfg.AppName + " - stopped")
	case StatusFFmpegMissing:
		startItem.Disable()
		stopItem.Disable()
		systray.SetTooltip(cfg.AppName + " - FFmpeg missing")
	}
	systray.SetIcon(iconFor(s))
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
