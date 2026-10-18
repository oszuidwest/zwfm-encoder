//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func installerSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func installerPreamble(t *testing.T) string {
	t.Helper()
	preamble, _, ok := strings.Cut(installerSource(t), "# Configuration\n")
	if !ok {
		t.Fatal("missing installer preamble")
	}
	return preamble
}

func TestInstallerSecretIsHiddenAndNeverExported(t *testing.T) {
	t.Parallel()
	_, prompt, ok := strings.Cut(installerSource(t), "  # Web password\n")
	if !ok {
		t.Fatal("missing password prompt")
	}
	prompt, _, ok = strings.Cut(prompt, "  # Web port\n")
	if !ok {
		t.Fatal("missing password prompt end")
	}
	// The shared library owns no-echo terminal handling; require its secret entry point.
	stub := `
prompt_secret() {
  [ "$#" = 2 ] && [ "$1" = WEB_PASSWORD ] || return 2
  WEB_PASSWORD=' hidden secret '
}
`
	cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+`
bash -c '[ "${WEB_PASSWORD+x}" != x ]'
`+stub+prompt+`
[ "$WEB_PASSWORD" = ' hidden secret ' ]
bash -c '[ "${WEB_PASSWORD+x}" != x ]'
`)
	cmd.Env = append(os.Environ(), "WEB_PASSWORD=inherited test secret")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("secret handling: %v\n%s", err, out)
	}
}

func TestInstallerFailureExplainsStoppedServiceWithoutLeakingCommand(t *testing.T) {
	t.Parallel()
	for _, stopped := range []string{"true", "false"} {
		t.Run(stopped, func(t *testing.T) {
			// A failing command substitution and an explicit status other than 1.
			cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+"\nENCODER_STOPPED="+stopped+"\nX=$(false) || exit 23\n")
			cmd.Env = append(os.Environ(), "WEB_PASSWORD=private-test-value")
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 || strings.Count(string(out), "Installation failed (exit 23)") != 1 ||
				strings.Contains(string(out), "private-test-value") {
				t.Fatalf("failure diagnostics: %v %s", err, out)
			}
			if strings.Contains(string(out), "encoder may still be stopped") != (stopped == "true") {
				t.Fatalf("incorrect stopped-service warning: %s", out)
			}
		})
	}
}

func TestInstallerCompletionUsesCapturedHelp(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	_, fragment, ok := strings.Cut(installerSource(t), "# Read port from config for display")
	if !ok {
		t.Fatal("completion block missing")
	}
	fragment, _, ok = strings.Cut(fragment, "if [ \"$KEEP_CONFIG\" == \"y\" ]; then")
	if !ok {
		t.Fatal("completion block end missing")
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"system":{"bind_address":"127.0.0.1","port":8080}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// INSTALL_DIR has no binary: the block must not probe it a second time.
	cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+"ENCODER_HELP='  -hash-password'\n#"+fragment)
	cmd.Env = append(os.Environ(), "INSTALL_DIR=/nonexistent", "CONFIG_FILE="+cfg, "FIRST_IP=192.0.2.1", "BOLD=", "NC=")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "http://127.0.0.1:8080") {
		t.Fatalf("completion: %v %s", err, out)
	}
}
