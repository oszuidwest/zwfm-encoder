// Package util provides utility functions for the encoder.
package util

import (
	"fmt"
	"math"
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

	return uint8(ri), uint8(gi), uint8(bi), nil
}

// RGBToHex converts RGB components to a hex color string (#RRGGBB).
func RGBToHex(r, g, b uint8) string {
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

// RelativeLuminance calculates the relative luminance of a color per WCAG 2.1.
// Returns a value between 0 (black) and 1 (white).
func RelativeLuminance(r, g, b uint8) float64 {
	// Convert to sRGB
	rs := float64(r) / 255.0
	gs := float64(g) / 255.0
	bs := float64(b) / 255.0

	// Apply gamma correction
	if rs <= 0.03928 {
		rs /= 12.92
	} else {
		rs = math.Pow((rs+0.055)/1.055, 2.4)
	}

	if gs <= 0.03928 {
		gs /= 12.92
	} else {
		gs = math.Pow((gs+0.055)/1.055, 2.4)
	}

	if bs <= 0.03928 {
		bs /= 12.92
	} else {
		bs = math.Pow((bs+0.055)/1.055, 2.4)
	}

	return 0.2126*rs + 0.7152*gs + 0.0722*bs
}

// ContrastRatio calculates the contrast ratio between two colors.
// Returns a value between 1 (no contrast) and 21 (maximum contrast).
func ContrastRatio(l1, l2 float64) float64 {
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// EnsureContrast adjusts a color to ensure minimum contrast ratio with white.
// Uses WCAG AA standard (4.5:1 for normal text).
// Returns the adjusted hex color.
func EnsureContrast(hex string) string {
	r, g, b, err := ParseHexColor(hex)
	if err != nil {
		return hex
	}

	whiteLum := 1.0
	colorLum := RelativeLuminance(r, g, b)
	ratio := ContrastRatio(whiteLum, colorLum)

	// WCAG AA requires 4.5:1 for normal text
	const minRatio = 4.5

	if ratio >= minRatio {
		return hex
	}

	// Darken the color until we meet the contrast ratio
	factor := 1.0
	for i := 0; i < 20; i++ {
		factor -= 0.05
		if factor < 0 {
			factor = 0
		}

		nr := uint8(float64(r) * factor)
		ng := uint8(float64(g) * factor)
		nb := uint8(float64(b) * factor)

		newLum := RelativeLuminance(nr, ng, nb)
		newRatio := ContrastRatio(whiteLum, newLum)

		if newRatio >= minRatio {
			return RGBToHex(nr, ng, nb)
		}
	}

	// Fallback: return very dark version
	return RGBToHex(uint8(float64(r)*0.3), uint8(float64(g)*0.3), uint8(float64(b)*0.3))
}

// DarkenColor darkens a hex color by a percentage (0-100).
func DarkenColor(hex string, percent int) string {
	r, g, b, err := ParseHexColor(hex)
	if err != nil {
		return hex
	}

	factor := 1.0 - float64(percent)/100.0
	if factor < 0 {
		factor = 0
	}

	return RGBToHex(
		uint8(float64(r)*factor),
		uint8(float64(g)*factor),
		uint8(float64(b)*factor),
	)
}

// GenerateBrandCSS generates the CSS custom properties for branding.
// Returns CSS that can be injected into the page.
func GenerateBrandCSS(primaryColor string) string {
	// Ensure the color has sufficient contrast with white
	safeColor := EnsureContrast(primaryColor)

	// Generate hover color (10% darker)
	hoverColor := DarkenColor(safeColor, 10)

	return fmt.Sprintf(":root{--brand:%s;--brand-hover:%s}", safeColor, hoverColor)
}
