package util

import (
	"strings"
	"testing"
)

func TestValidatePathRejectsTraversalAndAcceptsSafePath(t *testing.T) {
	t.Parallel()
	// Absolute-looking fixtures are intentional here: ValidatePath applies a
	// slash-based API policy independent of the host platform.
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "safe absolute path", path: "/recordings/hour.mp3"},
		{name: "empty path", wantErr: "path: is required"},
		{name: "parent directory reference", path: "/recordings/../escape.mp3", wantErr: "path: path cannot contain '..'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePath("path", tt.path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePath() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidatePath() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
