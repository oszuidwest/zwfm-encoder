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
			cmd.Env = append(os.Environ(), "INSTALL_DIR="+dir, "CONFIG_FILE="+path, "STATION_NAME=Test station", "WEB_USERNAME=admin", "WEB_PORT=8080", "WEB_BIND_ADDRESS=127.0.0.1", "WEB_PASSWORD=installer secret", "BLUE=", "GREEN=", "YELLOW=", "NC=", "AUTH_TEST_MODE="+mode, "AUTH_TEST_PASSWORD=installer secret", "AUTH_TEST_HASH="+hash)
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
			var cfg struct{ System config.SystemConfig }
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.System.BindAddress != "127.0.0.1" || cfg.System.Port != 8080 || cfg.System.Username != "admin" {
				t.Fatal("installer dropped listener/auth settings")
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

func TestInstallerHeartbeatCron(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, block, found := strings.Cut(string(script), "# Set up heartbeat monitoring if enabled")
	block, _, found2 := strings.Cut(block, "# COMPLETION")
	if !found || !found2 {
		t.Fatal("installer heartbeat block missing")
	}
	stub, err := os.ReadFile("tests/fixtures/crontab.sh")
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://heartbeat.example/abc"
	cronjob := "* * * * * curl -fsS --max-time 10 -o /dev/null '" + url + "' > /dev/null 2>&1\n"
	jobs := "# m h dom mon dow command\n0 3 * * * /usr/local/bin/backup\n"
	for _, tc := range []struct {
		name, existing, want string
		missing, readError   bool
	}{
		// Fresh Raspberry Pi OS: root has no crontab and `crontab -l` exits 1.
		{name: "no crontab", missing: true, want: cronjob},
		{name: "empty crontab", want: cronjob},
		{name: "unrelated jobs kept", existing: jobs, want: jobs + cronjob},
		// Any other read failure must leave the crontab alone and let the install continue.
		{name: "read error", existing: jobs, readError: true, want: jobs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			//nolint:gosec // G306: executable fake binary in an isolated test directory.
			if err := os.WriteFile(filepath.Join(dir, "crontab"), stub, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "crontab.txt")
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CRONTAB_TEST_FILE="+path, "CRONTAB_TEST_READ_ERROR=",
				"ENABLE_HEARTBEAT=y", "HEARTBEAT_URL="+url,
				"BLUE=", "GREEN=", "YELLOW=", "BOLD=", "NC=")
			// A re-run must find the job instead of adding it twice.
			runs := []string{"Heartbeat monitoring configured", "Heartbeat monitoring already configured"}
			if tc.readError {
				env = append(env, "CRONTAB_TEST_READ_ERROR=1")
				runs = []string{"Warning: could not read root's crontab, heartbeat monitoring not configured\nAdd this line with crontab -e:\n  " + cronjob}
			}
			for run, want := range runs {
				cmd := util.CommandContext(t.Context(), "bash", "-c", "set -euo pipefail\n"+block+"echo heartbeat step done\n")
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(out), want) || !strings.HasSuffix(string(out), "heartbeat step done\n") {
					t.Fatalf("run %d: %v\n%s", run+1, err, out)
				}
			}
			data, err := os.ReadFile(path) //nolint:gosec // G304: isolated crontab fixture under t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			if got := string(data); got != tc.want {
				t.Fatalf("crontab = %q, want %q", got, tc.want)
			}
		})
	}
}
