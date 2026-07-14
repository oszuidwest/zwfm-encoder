//go:build windows

package tray

import (
	"log/slog"
	"sync"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

// Run installs the tray icon and menu, then blocks until the user picks Quit
// or Quit is invoked externally. Must be called from the main goroutine:
// fyne.io/systray locks that goroutine to its OS thread at package init and
// runs the message loop there.
func Run(cfg Config) {
	// done stops the click and poll goroutines when the tray exits, so
	// nothing calls into systray after its message loop has ended.
	done := make(chan struct{})
	var handlers sync.WaitGroup

	// systray invokes onReady on its own goroutine and never waits for it
	// on Windows, so a very early Quit can make systray.Run return before
	// installMenu has run. The exited barrier closes that gap: by the time
	// handlers.Wait() executes, the handler goroutine is either registered
	// with the WaitGroup or will never be installed at all.
	var mu sync.Mutex
	exited := false

	systray.Run(
		func() {
			mu.Lock()
			defer mu.Unlock()
			if !exited {
				installMenu(cfg, done, &handlers)
			}
		},
		func() { close(done) },
	)

	mu.Lock()
	exited = true
	mu.Unlock()
	// An externally invoked Quit (signal bridge) can end the message loop
	// while a click handler is still inside StartEncoder/StopEncoder. Wait
	// for it, so the caller never runs shutdown concurrently with an
	// in-flight encoder operation.
	handlers.Wait()
}

// Quit signals the tray to exit; safe to call from any goroutine.
func Quit() {
	systray.Quit()
}

func installMenu(cfg Config, done <-chan struct{}, handlers *sync.WaitGroup) {
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
		systray.SetIcon(iconICO[s])
	}

	last := cfg.Status()
	apply(last)

	// Menu clicks. Handlers run synchronously on this goroutine: Stop can
	// block for a few seconds (it waits out the process shutdown timeout)
	// and queues later clicks behind it, but it guarantees no encoder
	// operation is ever in flight once this goroutine exits and Run's
	// handlers.Wait() unblocks into shutdown.
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			select {
			case <-openUI.ClickedCh:
				if err := shellOpen(cfg.URL); err != nil {
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
				if err := shellOpen(cfg.LogDir); err != nil {
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

// shellOpen opens target (a URL or a folder) with its default handler - the
// browser or Explorer - via ShellExecute, without spawning an intermediate
// process.
func shellOpen(target string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, t, nil, nil, windows.SW_SHOWNORMAL)
}
