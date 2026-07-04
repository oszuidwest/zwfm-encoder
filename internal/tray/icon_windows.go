//go:build windows

package tray

import (
	"bytes"
	"encoding/binary"
)

// iconICO is a 16x16 solid-color ICO used as the tray icon. Built once at
// package init so no binary asset ships in the repo. The station-configurable
// brand color would need a live config lookup which is not available before
// the tray is up, so the tray uses a fixed pink (#E6007E).
var iconICO = buildIcon()

func buildIcon() []byte {
	const (
		width  = 16
		height = 16
		// #E6007E in BGRA byte order.
		blue, green, red, alpha = 0x7E, 0x00, 0xE6, 0xFF
	)

	pixels := make([]byte, width*height*4)
	for i := 0; i+3 < len(pixels); i += 4 {
		pixels[i] = blue
		pixels[i+1] = green
		pixels[i+2] = red
		pixels[i+3] = alpha
	}
	andMask := make([]byte, width*height/8) // fully opaque

	var buf bytes.Buffer

	// ICONDIR (6 bytes).
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: 1 = icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // count

	// ICONDIRENTRY (16 bytes). Size is bounded by the constant 16x16 layout.
	bytesInRes := uint32(40 + len(pixels) + len(andMask)) //nolint:gosec // bounded by fixed 16x16 icon dimensions
	buf.WriteByte(width)
	buf.WriteByte(height)
	buf.WriteByte(0)                                        // color count
	buf.WriteByte(0)                                        // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitCount
	_ = binary.Write(&buf, binary.LittleEndian, bytesInRes)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22)) // image offset

	// BITMAPINFOHEADER (40 bytes). Height is doubled: pixel rows followed by
	// AND-mask rows, per the ICO on-disk layout.
	_ = binary.Write(&buf, binary.LittleEndian, uint32(40))
	_ = binary.Write(&buf, binary.LittleEndian, int32(width))
	_ = binary.Write(&buf, binary.LittleEndian, int32(height*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // BI_RGB
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // sizeImage (0 ok for BI_RGB)
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))

	buf.Write(pixels)
	buf.Write(andMask)

	return buf.Bytes()
}
