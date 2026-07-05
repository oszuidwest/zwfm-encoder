//go:build windows

package tray

import (
	_ "embed"
	"encoding/binary"
)

//go:embed icon_running.png
var iconRunningPNG []byte

//go:embed icon_stopped.png
var iconStoppedPNG []byte

//go:embed icon_error.png
var iconErrorPNG []byte

// iconICO holds the ICO-wrapped bytes for each Status, built once at
// package init.
var iconICO = map[Status][]byte{
	StatusRunning:       wrapPNGInICO(iconRunningPNG),
	StatusStopped:       wrapPNGInICO(iconStoppedPNG),
	StatusFFmpegMissing: wrapPNGInICO(iconErrorPNG),
}

// wrapPNGInICO builds a single-image ICO around a PNG payload. Windows Vista
// and later accept PNG-in-ICO natively, so no BMP conversion is needed.
func wrapPNGInICO(png []byte) []byte {
	// ICONDIR (reserved=0, type=1, count=1) + one ICONDIRENTRY (64x64,
	// 32-bit, 1 plane, image size, payload offset 22 = 6+16).
	hdr := []byte{
		0, 0, 1, 0, 1, 0,
		64, 64, 0, 0, 1, 0, 32, 0, 0, 0, 0, 0, 22, 0, 0, 0,
	}
	binary.LittleEndian.PutUint32(hdr[14:], uint32(len(png))) //nolint:gosec // PNG length fits in uint32 for our embedded icons
	return append(hdr, png...)
}
