//go:build windows

package tray

import (
	"log/slog"
	"time"

	"fyne.io/systray"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Run installs the tray icon and menu, then blocks until the user picks Quit
// or Quit is invoked externally. Must be called from the main goroutine:
// fyne.io/systray locks that goroutine to its OS thread at package init and
// runs the message loop there.
func Run(cfg Config) {
	// done stops the click and poll goroutines when the tray exits, so
	// nothing calls into systray after its message loop has ended.
	done := make(chan struct{})
	systray.Run(
		func() { installMenu(cfg, done) },
		func() { close(done) },
	)
}

// Quit signals the tray to exit; safe to call from any goroutine.
func Quit() {
	systray.Quit()
}

func installMenu(cfg Config, done <-chan struct{}) {
	openUI := systray.AddMenuItem("Open UI", "Open the web interface in the default browser")
	systray.AddSeparator()
	startItem := systray.AddMenuItem("Start Encoder", "Start audio capture and streaming")
	stopItem := systray.AddMenuItem("Stop Encoder", "Stop audio capture and streaming")
	systray.AddSeparator()
	showLogs := systray.AddMenuItem("Show Logs Folder", "Open the log folder in Explorer")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Exit the encoder")

	// apply grays out Start / Stop and swaps the tooltip and icon for the
	// given status.
	apply := func(s Status) {
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
		icon, ok := iconICO[s]
		if !ok { // unknown future status: any icon beats none
			icon = iconICO[StatusStopped]
		}
		systray.SetIcon(icon)
	}

	last := cfg.Status()
	apply(last)

	go func() { // menu clicks
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
			case <-done:
				return
			}
		}
	}()

	// Poll the encoder status and reapply on change. Polling is fine here -
	// status changes are rare and the tray is not on any hot path. Applying
	// only on change also lets an error tooltip set by the click handler
	// persist until the status actually moves.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if s := cfg.Status(); s != last {
					apply(s)
					last = s
				}
			case <-done:
				return
			}
		}
	}()
}

// openBrowser launches the default browser via rundll32.
func openBrowser(url string) error {
	return util.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// openFolder opens the given directory in Explorer.
func openFolder(dir string) error {
	return util.Command("explorer.exe", dir).Start()
}
