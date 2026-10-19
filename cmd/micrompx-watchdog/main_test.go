package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIHelpAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"-help"}, 0}, {"unknown_flag", []string{"-restart"}, 2}, {"unexpected_argument", []string{"start"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var output, diagnostics bytes.Buffer
			if got := run(t.Context(), tc.args, &output, &diagnostics); got != tc.want {
				t.Fatalf("exit=%d, want=%d: %s", got, tc.want, diagnostics.String())
			}
		})
	}
}

func TestCLICheckHasNoStateOrMailSideEffects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"site":"Test transmitter", "state_dir":"` + filepath.ToSlash(filepath.Join(dir, "state")) + `"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"-config", path, "-check"}, &output, &diagnostics); code != 0 {
		t.Fatalf("exit=%d: %s", code, diagnostics.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("check mode touched state")
	}
}
