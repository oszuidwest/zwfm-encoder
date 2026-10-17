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
