package util

import (
	"path/filepath"
	"testing"
)

func TestWindowsDataDir(t *testing.T) {
	// Variable rather than inline literal: gocritic's filepathJoin check
	// rejects literal Join arguments containing a separator.
	defaultRoot := `C:\ProgramData`
	customRoot := filepath.FromSlash("D:/CustomData")

	tests := []struct {
		name        string
		programData string
		expected    string
	}{
		{
			name:        "uses programdata environment variable",
			programData: customRoot,
			expected:    filepath.Join(customRoot, "encoder"),
		},
		{
			name:        "defaults to programdata root when unset",
			programData: "",
			expected:    filepath.Join(defaultRoot, "encoder"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROGRAMDATA", tt.programData)
			if got := WindowsDataDir(); got != tt.expected {
				t.Errorf("WindowsDataDir() = %q, want %q", got, tt.expected)
			}
		})
	}
}
