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
	t.Run("success", func(t *testing.T) {
		cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+"\nENCODER_STOPPED=true\nexit 0\n")
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("successful exit: %v %q", err, out)
		}
	})
}

func TestInstallerWarnsWhenTheStopFails(t *testing.T) {
	t.Parallel()
	_, stop, ok := strings.Cut(installerSource(t), "# Stop existing service if running\n")
	if ok {
		stop, _, ok = strings.Cut(stop, "mv -f ")
	}
	if !ok {
		t.Fatal("stop block missing")
	}
	bin := t.TempDir()
	for name, script := range map[string]string{
		"systemctl": "#!/bin/sh\n[ \"$1\" = is-active ]\n",
		"pkill":     "#!/bin/sh\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // G306: a PATH stub must be executable.
			t.Fatal(err)
		}
	}
	cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+stop)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "BLUE=", "YELLOW=", "NC=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "encoder may still be stopped") {
		t.Fatalf("failed stop: %v %s", err, out)
	}
}

func TestInstallerCompletionUsesCapturedHelp(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	source := installerSource(t)
	// One probe, of the downloaded binary, before the configuration needs it.
	probe := strings.Index(source, "ENCODER_HELP=$(\"$ENCODER_CANDIDATE\" -help 2>&1)")
	if strings.Count(source, " -help ") != 1 || probe < 0 || probe > strings.Index(source, "  # Generate configuration file using jq") {
		t.Fatal("encoder -help must be captured once, from the downloaded binary, before the configuration is written")
	}
	_, fragment, ok := strings.Cut(source, "# Read port from config for display")
	if !ok {
		t.Fatal("completion block missing")
	}
	fragment, _, ok = strings.Cut(fragment, "if [ \"$KEEP_CONFIG\" == \"y\" ]; then")
	if !ok {
		t.Fatal("completion block end missing")
	}
	for _, tc := range []struct{ config, url, hint string }{
		{`{"system":{"bind_address":"127.0.0.1","port":8080}}`, "http://127.0.0.1:8080", "(local only)"},
		{`{"system":{"bind_address":"0.0.0.0","port":8080}}`, "http://192.0.2.1:8080", ""},
		{`{"system":{"bind_address":"::","port":8080}}`, "http://192.0.2.1:8080", ""},
		{`{"system":{"port":8080}}`, "http://192.0.2.1:8080", ""},
		{`{"system":{"bind_address":"192.0.2.1","port":8080}}`, "http://192.0.2.1:8080", "(check configured bind address)"},
		{`not json`, "http://192.0.2.1:8080", ""},
	} {
		cfg := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfg, []byte(tc.config), 0o600); err != nil {
			t.Fatal(err)
		}
		// INSTALL_DIR has no binary: the block must not probe it a second time.
		cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+"ENCODER_HELP='  -hash-password'\n#"+fragment)
		cmd.Env = append(os.Environ(), "INSTALL_DIR=/nonexistent", "CONFIG_FILE="+cfg, "FIRST_IP=192.0.2.1", "BOLD=", "NC=")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), tc.url) || strings.Contains(string(out), "(check configured bind address)") != (tc.hint == "(check configured bind address)") ||
			!strings.Contains(string(out), tc.hint) {
			t.Fatalf("completion for %s: %v %s", tc.config, err, out)
		}
	}
}

func TestInstallerRefusesReleaseThatCannotReadKeptConfig(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	_, guard, ok := strings.Cut(installerSource(t), "# Refuse a release that cannot read the kept configuration\n")
	if ok {
		guard, _, ok = strings.Cut(guard, "# Stop existing service if running\n")
	}
	if !ok {
		t.Fatal("downgrade guard missing")
	}
	for _, tc := range []struct {
		name, config, help string
		refused, legacy    bool
	}{
		{"hashed config with an older release", `{"system":{"password_hash":"x"}}`, "  -config", true, false},
		{"hashed legacy config with an older release", `{"system":{"password_hash":"x"}}`, "  -config", true, true},
		{"hashed config with a current release", `{"system":{"password_hash":"x"}}`, "  -hash-password", false, false},
		{"plaintext config with an older release", `{"system":{"password":"x"}}`, "  -config", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			installDir, configDir := t.TempDir(), t.TempDir()
			cfg := filepath.Join(configDir, "config.json")
			written := cfg
			if tc.legacy {
				written = filepath.Join(installDir, "config.json")
			}
			if err := os.WriteFile(written, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := util.CommandContext(t.Context(), "bash", "-c", installerPreamble(t)+guard)
			cmd.Env = append(os.Environ(), "KEEP_CONFIG=y", "CONFIG_FILE="+cfg, "INSTALL_DIR="+installDir, "ENCODER_HELP="+tc.help,
				"LATEST_RELEASE=v0.0.1", "RED=", "NC=")
			out, err := cmd.CombinedOutput()
			if refused := err != nil; refused != tc.refused || tc.refused && !strings.Contains(string(out), "left untouched") {
				t.Fatalf("guard = %v %s, want refused=%t", err, out, tc.refused)
			}
		})
	}
}
