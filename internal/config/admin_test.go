package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestAdminPasswordHashAndBindAddress(t *testing.T) {
	t.Parallel()
	hash, err := util.HashPassword("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, bind, password, hash, wantErr string }{
		{name: "legacy", password: "test-secret"},
		{name: "hashed loopback", bind: "127.0.0.1", hash: hash},
		{name: "IPv6", bind: "::1", hash: hash},
		{name: "explicit wildcard", bind: "0.0.0.0", hash: hash},
		{name: "hostname rejected", bind: "localhost", hash: hash, wantErr: "system.bind_address"},
		{name: "port rejected", bind: "127.0.0.1:8080", hash: hash, wantErr: "system.bind_address"},
		{name: "ambiguous credentials", password: "test-secret", hash: hash, wantErr: "remove plaintext"},
		{name: "malformed hash", hash: "broken", wantErr: "system.password_hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig("")
			cfg.System.BindAddress, cfg.System.Password, cfg.System.PasswordHash = tc.bind, tc.password, tc.hash
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := writeConfig(t, string(data))
			loaded := New(path)
			err = loaded.Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot := loaded.Snapshot()
			if snapshot.WebPassword != tc.password || snapshot.WebPasswordHash != tc.hash || snapshot.WebBindAddress != tc.bind {
				t.Fatal("admin config did not reach snapshot")
			}
			before, err := os.ReadFile(path) //nolint:gosec // G304: path is a config fixture inside t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, data) {
				t.Fatal("loading a legacy config must not rewrite it")
			}
			if err := loaded.SetRecordingAPIKey("test"); err != nil {
				t.Fatal(err)
			}
			reloaded := New(path)
			if err := reloaded.Load(); err != nil {
				t.Fatal(err)
			}
			if got := reloaded.Snapshot(); got.WebPasswordHash != tc.hash || got.WebBindAddress != tc.bind {
				t.Fatal("saving lost the admin hash or bind")
			}
		})
	}
}

func TestNewConfigUsesHashedPasswordAndLoopback(t *testing.T) {
	t.Parallel()
	cfg, path := newLoadedConfig(t)
	snapshot := cfg.Snapshot()
	if snapshot.WebBindAddress != "127.0.0.1" || snapshot.WebPassword != "" || !util.VerifyPasswordHash(DefaultWebPassword, snapshot.WebPasswordHash) {
		t.Fatal("new config is not loopback-bound with a hashed credential")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a config fixture inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"password":`) {
		t.Fatal("new config persisted plaintext")
	}
}

func TestExplicitNullBindAddressIsRejected(t *testing.T) {
	t.Parallel()
	assertLoadErrorContains(t, `{"system":{"port":8080,"username":"admin","password":"encoder","bind_address":null}}`, "system.bind_address: must not be null")
}
