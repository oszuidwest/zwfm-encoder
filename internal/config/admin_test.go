package config

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestAdminPasswordAndBindAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, bind, password, wantErr string }{
		{name: "default bind", password: "test-secret"},
		{name: "loopback", bind: "127.0.0.1", password: "test-secret"},
		{name: "private interface", bind: "192.168.1.20", password: "test-secret"},
		{name: "IPv6", bind: "::1", password: "test-secret"},
		{name: "explicit wildcard", bind: "0.0.0.0", password: "test-secret"},
		{name: "hostname rejected", bind: "localhost", password: "test-secret", wantErr: "system.bind_address"},
		{name: "port rejected", bind: "127.0.0.1:8080", password: "test-secret", wantErr: "system.bind_address"},
		{name: "password required", wantErr: "system.password_hash is no longer supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig("")
			cfg.System.BindAddress, cfg.System.Password = tc.bind, tc.password
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
			if snapshot.WebPassword != tc.password || snapshot.WebBindAddress != tc.bind {
				t.Fatal("admin config did not reach snapshot")
			}
			before, err := os.ReadFile(path) //nolint:gosec // G304: path is a config fixture inside t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, data) {
				t.Fatal("loading a config must not rewrite it")
			}
			if err := loaded.SetRecordingAPIKey("test"); err != nil {
				t.Fatal(err)
			}
			reloaded := New(path)
			if err := reloaded.Load(); err != nil {
				t.Fatal(err)
			}
			if got := reloaded.Snapshot(); got.WebPassword != tc.password || got.WebBindAddress != tc.bind {
				t.Fatal("saving lost the admin password or bind")
			}
		})
	}
}

func TestNewConfigUsesDefaultPasswordAndLANAccess(t *testing.T) {
	t.Parallel()
	cfg, _ := newLoadedConfig(t)
	if snapshot := cfg.Snapshot(); snapshot.WebBindAddress != "0.0.0.0" || snapshot.WebPassword != DefaultWebPassword {
		t.Fatal("new config must allow direct LAN access with the default password")
	}
}

func TestExplicitNullBindAddressIsRejected(t *testing.T) {
	t.Parallel()
	assertLoadErrorContains(t, `{"system":{"port":8080,"username":"admin","password":"encoder","bind_address":null}}`, "system.bind_address: must not be null")
}

func TestTrustedProxyConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		proxies []string
		valid   bool
	}{
		{name: "none", valid: true},
		{name: "explicit peers", proxies: []string{"127.0.0.1/32", "::1/128", "10.0.0.2/32"}, valid: true},
		{name: "hostname", proxies: []string{"localhost"}},
		{name: "missing prefix", proxies: []string{"127.0.0.1"}},
		{name: "all IPv4", proxies: []string{"0.0.0.0/0"}},
		{name: "all IPv6", proxies: []string{"::/0"}},
		{name: "mapped proxy", proxies: []string{"::ffff:10.0.0.2/128"}},
		{name: "mapped all IPv4", proxies: []string{"::ffff:0:0/96"}},
		{name: "mapped broad prefix", proxies: []string{"::ffff:0:0/80"}},
		{name: "empty entry", proxies: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig("")
			cfg.System.TrustedProxies = tc.proxies
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := writeConfig(t, string(data))
			loaded := New(path)
			err = loaded.Load()
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "system.trusted_proxies") {
					t.Fatalf("Load=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot := loaded.Snapshot()
			if len(snapshot.TrustedProxies) != len(tc.proxies) {
				t.Fatal("proxy config did not reach snapshot")
			}
			if len(snapshot.TrustedProxies) > 0 {
				snapshot.TrustedProxies[0] = "0.0.0.0/0"
				if loaded.Snapshot().TrustedProxies[0] != tc.proxies[0] {
					t.Fatal("snapshot aliases trusted-proxy config")
				}
			}
			if err := loaded.SetRecordingAPIKey("test"); err != nil {
				t.Fatal(err)
			}
			reloaded := New(path)
			if err := reloaded.Load(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(reloaded.Snapshot().TrustedProxies, tc.proxies) {
				t.Fatal("saving lost trusted proxies")
			}
		})
	}
}
