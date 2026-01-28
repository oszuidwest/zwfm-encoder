package util

import (
	"fmt"
	"strings"
)

// ParseHexColor parses a hex color string (#RRGGBB) into RGB components.
func ParseHexColor(hex string) (r, g, b uint8, err error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, 0, fmt.Errorf("invalid hex color length: %s", hex)
	}

	var ri, gi, bi int
	_, err = fmt.Sscanf(hex, "%02x%02x%02x", &ri, &gi, &bi)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid hex color: %s", hex)
	}

	return uint8(ri), uint8(gi), uint8(bi), nil //nolint:gosec // Values are validated to be 0-255 by hex parsing
}

// RGBToHex converts RGB components to a hex color string (#RRGGBB).
func RGBToHex(r, g, b uint8) string {
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

// DarkenColor darkens a hex color by a percentage (0-100).
func DarkenColor(hex string, percent int) string {
	r, g, b, err := ParseHexColor(hex)
	if err != nil {
		return hex
	}

	factor := max(1.0-float64(percent)/100.0, 0.0)

	return RGBToHex(
		uint8(float64(r)*factor),
		uint8(float64(g)*factor),
		uint8(float64(b)*factor),
	)
}

// GenerateBrandCSS generates CSS custom properties for branding.
func GenerateBrandCSS(colorLight, colorDark string) string {
	hoverLight := DarkenColor(colorLight, 10)
	hoverDark := DarkenColor(colorDark, 10)

	return fmt.Sprintf(
		":root{--brand-light:%s;--brand-dark:%s;--brand:%s;--brand-hover:%s}"+
			"@media(prefers-color-scheme:dark){:root{--brand:%s;--brand-hover:%s}}",
		colorLight, colorDark, colorLight, hoverLight, colorDark, hoverDark,
	)
}
