//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestInstallerAdminConfig(t *testing.T) {
	t.Parallel()
	fragment := installerAdminFragment(t)
	stub, err := os.ReadFile("tests/fixtures/encoder-auth.sh")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := util.HashPassword("installer secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"hashed", "legacy", "fail", "help-fail"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			//nolint:gosec // G306: executable fake binary in an isolated test directory.
			if err := os.WriteFile(filepath.Join(dir, "encoder"), stub, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			cmd := util.CommandContext(t.Context(), "bash", "-c", fragment)
			cmd.Env = append(os.Environ(), "HEARTBEAT_URL=https://heartbeat.example/m1-token", "INSTALL_DIR="+dir, "CONFIG_FILE="+path, "STATION_NAME=Test station", "WEB_USERNAME=admin", "WEB_PORT=8080", "WEB_BIND_ADDRESS=127.0.0.1", "WEB_PASSWORD=installer secret", "BLUE=", "GREEN=", "YELLOW=", "NC=", "AUTH_TEST_MODE="+mode, "AUTH_TEST_PASSWORD=installer secret", "AUTH_TEST_HASH="+hash)
			out, err := cmd.CombinedOutput()
			if strings.Contains(mode, "fail") {
				if err == nil {
					t.Fatal("hashing failure fell back to plaintext")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("hashing failure wrote a config")
				}
				return
			}
			if err != nil {
				t.Fatalf("installer config: %v\n%s", err, out)
			}
			data, err := os.ReadFile(path) //nolint:gosec // G304: isolated config fixture under t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			var cfg config.ConfigData
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			got := [...]any{cfg.System.BindAddress, cfg.System.Port, cfg.System.Username, cfg.Notifications.Heartbeat.URL}
			if want := [...]any{"127.0.0.1", 8080, "admin", "https://heartbeat.example/m1-token"}; got != want {
				t.Fatalf("installer listener/auth/heartbeat settings = %v, want %v", got, want)
			}
			if mode == "hashed" {
				if cfg.System.Password != "" || cfg.System.PasswordHash != hash {
					t.Fatal("new release stored plaintext or lost the hash")
				}
			} else if cfg.System.Password != "installer secret" || cfg.System.PasswordHash != "" || !strings.Contains(string(out), "ignores the bind IP") {
				t.Fatal("older release compatibility must warn and preserve its credential format")
			}
		})
	}
}

func installerAdminFragment(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is needed for the installer test", tool)
		}
	}
	script, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, fragment, found := strings.Cut(string(script), "  # Generate configuration file using jq")
	if !found {
		t.Fatal("installer config block not found")
	}
	fragment, _, found = strings.Cut(fragment, "  # Set proper ownership and permissions")
	if !found {
		t.Fatal("installer config block end not found")
	}
	// Exercise the actual block, without running root/service/network operations.
	return "set -euo pipefail\n# Generate configuration file using jq" + fragment
}

func TestInstallerBindChoice(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, fragment, found := strings.Cut(string(script), "  # Direct LAN/VPN access is the normal appliance setup.")
	if !found {
		t.Fatal("installer bind prompt missing")
	}
	fragment, _, found = strings.Cut(fragment, "  # Timezone with validation")
	if !found {
		t.Fatal("installer bind prompt end missing")
	}
	// The shared prompt owns y/n validation; require that contract, not a free-form string.
	stub := `set -euo pipefail
prompt_user() {
  [ "$4" = "y/n" ] || exit 1
  choice="${AUTH_TEST_EXPOSE:-$2}"
  case "$choice" in
    y|n) printf -v "$1" '%s' "$choice" ;;
    *) exit 2 ;;
  esac
}
`
	for _, tc := range []struct{ name, input, want string }{
		{name: "default LAN access", want: "0.0.0.0"},
		{name: "local only", input: "n", want: "127.0.0.1"},
		{name: "explicit LAN access", input: "y", want: "0.0.0.0"},
		{name: "hostname rejected", input: "localhost"},
		{name: "address with port rejected", input: "127.0.0.1:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := util.CommandContext(t.Context(), "bash", "-c", stub+fragment+`printf '%s' "$WEB_BIND_ADDRESS"`)
			cmd.Env = append(os.Environ(), "AUTH_TEST_EXPOSE="+tc.input)
			out, err := cmd.CombinedOutput()
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid free-form choice accepted")
				}
				return
			}
			if err != nil || string(out) != tc.want {
				t.Fatalf("installer bind = %q, %v, want %q", out, err, tc.want)
			}
		})
	}
}

// An update keeps the running encoder until the new one is downloaded and runs.
func TestInstallerReplacesEncoderOnlyWhenNewOneRuns(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, fragment, found := strings.Cut(string(script), "# The running encoder stays up until the new one is in place and runs.")
	fragment, _, found2 := strings.Cut(fragment, "# Create config directory")
	if !found || !found2 {
		t.Fatal("installer update block not found")
	}
	stubs := `set -euo pipefail
file_download() { [ "$NEW_ENCODER" = missing ] && return 1; printf '#!/bin/sh\nexit %s\n' "$NEW_ENCODER" > "$2"; }
systemctl() { echo "$*" >> "$INSTALL_DIR/calls"; }
pkill() { return 1; }
`
	for _, tc := range []struct{ newEncoder, want string }{{"missing", "old"}, {"1", "old"}, {"0", "new"}} {
		dir := t.TempDir()
		old := filepath.Join(dir, "encoder")
		if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := util.CommandContext(t.Context(), "bash", "-c", stubs+fragment)
		cmd.Env = append(os.Environ(), "INSTALL_DIR="+dir, "SERVICE_PATH="+filepath.Join(dir, "unit"), "NEW_ENCODER="+tc.newEncoder, "ENCODER_BINARY_URL=", "ENCODER_SERVICE_URL=", "RED=", "BLUE=", "YELLOW=", "NC=")
		out, runErr := cmd.CombinedOutput()
		data, _ := os.ReadFile(old)                          //nolint:gosec // G304: isolated fixture under t.TempDir.
		calls, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // G304: isolated fixture under t.TempDir.
		replaced := strings.HasPrefix(string(data), "#!/bin/sh")
		if replaced != (tc.want == "new") || (runErr == nil) != (tc.want == "new") || (len(calls) > 0) != (tc.want == "new") {
			t.Errorf("new encoder %s: replaced %t, stop calls %q, err %v\n%s", tc.newEncoder, replaced, calls, runErr, out)
		}
	}
}
