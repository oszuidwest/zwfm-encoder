package decodermonitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigDefaultsAndSafeValidation(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig(privateConfig(t, `{"site":"Test transmitter"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SendMail || cfg.PollSeconds != 10 || cfg.RecoverySeconds != 900 {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
	for _, content := range []string{
		`{"site":"a","site":"b"}`, `{"site":"a","send_mail":null}`, `{"site":"a","reset":true}`,
		`{"site":"a","poll_seconds":0}`, `{"site":"a","critical_count":1000000}`,
		`{"site":"a","stalled_checks":1}`, `{"site":"a","state_dir":"/"}`,
		`{"site":"a","pcm_status":"/dev/snd/pcmC1D0p"}`, `{"site":"a","send_mail":true}`,
		`{"site":"a","heartbeat_url":"http://example.invalid/secret"}`, `{"site":"a","heartbeat_url":"https://user:secret@example.invalid"}`,
		`{"site":"a"} {}`, `[]`, `null`, `{"site":"a","poll_seconds":"do-not-leak-this-secret"}`,
	} {
		if _, err := LoadConfig(privateConfig(t, content)); err == nil || strings.Contains(err.Error(), "do-not-leak-this-secret") {
			t.Fatalf("invalid configuration accepted or secret echoed: %s: %v", content, err)
		}
	}
}

func TestConfigRejectsMissingPublicAndOversizeFiles(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := LoadConfig(missing); err == nil {
		t.Fatal("missing config accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("configuration was created unexpectedly")
	}
	path := privateConfig(t, `{"site":"a"}`)
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // Deliberately public fixture must be rejected.
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("world-readable config accepted")
	}
	if _, err := LoadConfig(privateConfig(t, strings.Repeat(" ", 65537))); err == nil {
		t.Fatal("oversize config accepted")
	}
}
