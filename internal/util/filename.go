package util

import "strings"

// SanitizeFilename converts a recorder name to its recording filename prefix.
func SanitizeFilename(name string) string {
	result := strings.Map(func(c rune) rune {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_':
			return c
		case c == ' ':
			return '-'
		default:
			return -1
		}
	}, name)
	if result == "" {
		return "recording"
	}
	return result
}
