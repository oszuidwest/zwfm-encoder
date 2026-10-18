package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, host, origin, site string
		want                     bool
	}{
		{name: "nonbrowser without origin", host: "encoder.local", want: true},
		{name: "same origin", host: "encoder.local:8080", origin: "http://encoder.local:8080", want: true},
		{name: "fetch metadata same origin", host: "encoder.local:8080", origin: "http://encoder.local:8080", site: "same-origin", want: true},
		// A proxy preserving Host needs no X-Forwarded-Proto: the browser decides.
		{name: "TLS proxy preserving Host", host: "encoder.example", origin: "https://encoder.example", site: "same-origin", want: true},
		{name: "scheme downgrade", host: "encoder.local", origin: "http://encoder.local", site: "same-site"},
		{name: "sibling site", host: "encoder.example", origin: "https://evil.example", site: "same-site"},
		{name: "cross site", host: "encoder.local", origin: "https://example.com", site: "cross-site"},
		{name: "different port", host: "encoder.local:8080", origin: "http://encoder.local:8081"},
		{name: "unrelated loopback", host: "encoder.local", origin: "http://127.0.0.1:8080"},
		{name: "unrelated private IPv4", host: "encoder.local", origin: "http://192.168.1.20:8080"},
		{name: "unrelated private IPv6", host: "encoder.local", origin: "http://[fd00::1]:8080"},
		{name: "same private IP", host: "192.168.1.20:8080", origin: "http://192.168.1.20:8080", want: true},
		{name: "same local tunnel origin", host: "127.0.0.1:8080", origin: "http://127.0.0.1:8080", want: true},
		{name: "null origin", host: "encoder.local", origin: "null"},
		{name: "invalid URL", host: "encoder.local", origin: "://bad-origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.site != "" {
				req.Header.Set("Sec-Fetch-Site", tt.site)
			}
			if got := checkOrigin(req); got != tt.want {
				t.Fatalf("checkOrigin = %t, want %t", got, tt.want)
			}
			if req.Method != http.MethodGet {
				t.Fatal("checkOrigin mutated the request")
			}
		})
	}
}
