//go:build !windows

package main

import (
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
	// The shared library owns no-echo terminal handling. Require its secret
	// entry point, and re-export in the stub to test defensive de-exporting too.
	stub := `
prompt_secret() {
  [ "$#" = 2 ] && [ "$1" = WEB_PASSWORD ] || return 2
  WEB_PASSWORD=' hidden secret '
  export WEB_PASSWORD
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
			cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+"\nENCODER_STOPPED="+stopped+"\nfalse\n")
			cmd.Env = append(os.Environ(), "WEB_PASSWORD=private-test-value")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "Installation failed (exit 1)") || strings.Contains(string(out), "private-test-value") {
				t.Fatalf("failure diagnostics: %v %s", err, out)
			}
			if strings.Contains(string(out), "encoder may still be stopped") != (stopped == "true") {
				t.Fatalf("incorrect stopped-service warning: %s", out)
			}
		})
	}
}

func TestInstallerCompletionReusesCapturedHelp(t *testing.T) {
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
	stub, err := os.ReadFile("tests/fixtures/encoder-auth.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "update captures once", true: "fresh install reuses capture"}[cached], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "encoder"), stub, 0o700); err != nil { //nolint:gosec // Isolated executable test fixture.
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfg, []byte(`{"system":{"bind_address":"127.0.0.1","port":8080}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			count := filepath.Join(dir, "help-count")
			preamble := installerPreamble(t)
			if cached {
				preamble += "\nENCODER_HELP='  -hash-password'\n"
			}
			cmd := util.CommandContext(t.Context(), "bash", "-c", preamble+"\n# Read port from config for display"+fragment)
			cmd.Env = append(os.Environ(), "INSTALL_DIR="+dir, "CONFIG_FILE="+cfg, "AUTH_TEST_HELP_COUNT="+count, "AUTH_TEST_MODE=hashed", "FIRST_IP=192.0.2.1", "BOLD=", "NC=", "WEB_PASSWORD=private-test-value")
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "http://127.0.0.1:8080") {
				t.Fatalf("completion: %v %s", err, out)
			}
			calls, err := os.ReadFile(count) //nolint:gosec // Isolated call-count fixture.
			if cached {
				if !os.IsNotExist(err) {
					t.Fatalf("cached help reran encoder: %s %v", calls, err)
				}
			} else if err != nil || string(calls) != "help\n" {
				t.Fatalf("help calls=%q %v", calls, err)
			}
		})
	}
}
