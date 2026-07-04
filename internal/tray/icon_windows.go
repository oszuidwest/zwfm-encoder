//go:build windows

package tray

import (
	"bytes"
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

// iconFor returns the ICO bytes for the given status; falls back to the
// stopped icon if the status is unknown.
func iconFor(s Status) []byte {
	if b, ok := iconICO[s]; ok {
		return b
	}
	return iconICO[StatusStopped]
}

// wrapPNGInICO builds a single-image ICO around a PNG payload. Windows Vista
// and later accept PNG-in-ICO natively, so no BMP conversion is needed.
func wrapPNGInICO(png []byte) []byte {
	const (
		iconDirSize   = 6
		iconEntrySize = 16
		width         = 64
		height        = 64
	)

	// The width/height bytes are 0 to mean 256; anything else is the literal
	// value. 64 fits in one byte.
	widthByte := byte(width)
	if width >= 256 {
		widthByte = 0
	}
	heightByte := byte(height)
	if height >= 256 {
		heightByte = 0
	}

	var buf bytes.Buffer
	// ICONDIR.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count

	// ICONDIRENTRY.
	buf.WriteByte(widthByte)
	buf.WriteByte(heightByte)
	buf.WriteByte(0)                                              // color count
	buf.WriteByte(0)                                              // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))        // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))       // bitCount
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(png))) //nolint:gosec // PNG length fits in uint32 for our embedded icons
	_ = binary.Write(&buf, binary.LittleEndian, uint32(iconDirSize+iconEntrySize))

	buf.Write(png)
	return buf.Bytes()
}
