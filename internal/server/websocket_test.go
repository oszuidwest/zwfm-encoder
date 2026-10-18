package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, host, origin, forwarded string
		tls                           bool
		trusted                       []string
		want                          bool
	}{
		{name: "nonbrowser without origin", host: "encoder.local", want: true},
		{name: "same HTTP origin", host: "encoder.local:8080", origin: "http://encoder.local:8080", want: true},
		{name: "default HTTP port", host: "encoder.local", origin: "http://encoder.local:80", want: true},
		{name: "default HTTPS port", host: "encoder.local:443", origin: "https://ENCODER.local", tls: true, want: true},
		{name: "different port", host: "encoder.local:8080", origin: "http://encoder.local:8081"},
		{name: "different scheme", host: "encoder.local:8080", origin: "https://encoder.local:8080"},
		{name: "TLS downgrade", host: "encoder.local", origin: "http://encoder.local", tls: true},
		{name: "unrelated localhost", host: "encoder.local", origin: "http://localhost:8080"},
		{name: "unrelated loopback", host: "encoder.local", origin: "http://127.0.0.1:8080"},
		{name: "unrelated IPv6 loopback", host: "encoder.local", origin: "http://[::1]:8080"},
		{name: "unrelated private IPv4", host: "encoder.local", origin: "http://192.168.1.20:8080"},
		{name: "unrelated private IPv6", host: "encoder.local", origin: "http://[fd00::1]:8080"},
		{name: "same private IP", host: "192.168.1.20:8080", origin: "http://192.168.1.20:8080", want: true},
		{name: "same IPv6 canonical address", host: "[fd00::1]:8080", origin: "http://[fd00:0:0:0:0:0:0:1]:8080", want: true},
		{name: "same local tunnel origin", host: "127.0.0.1:8080", origin: "http://127.0.0.1:8080", want: true},
		{name: "external origin", host: "encoder.local", origin: "https://example.com"},
		{name: "null origin", host: "encoder.local", origin: "null"},
		{name: "invalid URL", host: "encoder.local", origin: "://bad-origin"},
		{name: "userinfo", host: "encoder.local", origin: "http://attacker@encoder.local"},
		{name: "path", host: "encoder.local", origin: "http://encoder.local/"},
		{name: "query", host: "encoder.local", origin: "http://encoder.local?"},
		{name: "fragment", host: "encoder.local", origin: "http://encoder.local#"},
		{name: "oversized port", host: "encoder.local:999999", origin: "http://encoder.local:999999"},
		{name: "empty explicit port", host: "encoder.local", origin: "http://encoder.local:"},
		{name: "trusted TLS proxy", host: "encoder.example", origin: "https://encoder.example", forwarded: "https", trusted: []string{"192.0.2.1/32"}, want: true},
		{name: "untrusted forwarded scheme", host: "encoder.example", origin: "https://encoder.example", forwarded: "https"},
		{name: "bad forwarded scheme", host: "encoder.example", origin: "https://encoder.example", forwarded: "https,http", trusted: []string{"192.0.2.1/32"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)
			req.RemoteAddr, req.Host = "192.0.2.1:1234", tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if got := checkOrigin(req, tt.trusted); got != tt.want {
				t.Fatalf("checkOrigin = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestCheckOriginRejectsRepeatedHeaders(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"Origin", "X-Forwarded-Proto"} {
		r := httptest.NewRequest(http.MethodGet, "http://encoder.local/ws", http.NoBody)
		r.Header.Set("Origin", "https://encoder.local")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Add(header, r.Header.Get(header))
		if checkOrigin(r, []string{"192.0.2.1/32"}) {
			t.Fatalf("accepted repeated %s", header)
		}
	}
}
