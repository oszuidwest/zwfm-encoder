// Package tray provides a Windows systray for the encoder. On non-Windows
// platforms the package compiles to just the shared type declarations; the
// Run and Quit entry points only exist on Windows.
package tray

// Status represents the encoder's high-level state for tray display.
type Status int

const (
	// StatusStopped indicates the encoder is not currently running.
	StatusStopped Status = iota
	// StatusRunning indicates the encoder is actively processing audio.
	StatusRunning
	// StatusFFmpegMissing indicates FFmpeg was not found - encoder cannot start.
	StatusFFmpegMissing
)

// Config carries the callbacks and metadata that the tray menu needs. All
// callbacks must be non-nil.
type Config struct {
	// AppName is shown in the tray tooltip prefix.
	AppName string
	// URL is opened in the default browser by the Open UI menu item.
	URL string
	// LogDir is the folder opened by the Show Logs menu item.
	LogDir string
	// StartEncoder is invoked by the Start Encoder menu item.
	StartEncoder func() error
	// StopEncoder is invoked by the Stop Encoder menu item.
	StopEncoder func() error
	// Status returns the current encoder status, polled to update menu items.
	Status func() Status
}
