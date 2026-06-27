package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{
			name: "empty origin is allowed",
			host: "encoder.local",
			want: true,
		},
		{
			name:   "localhost origin is allowed",
			host:   "encoder.local",
			origin: "http://localhost:8080",
			want:   true,
		},
		{
			name:   "ipv4 loopback origin is allowed",
			host:   "encoder.local",
			origin: "http://127.0.0.1:8080",
			want:   true,
		},
		{
			name:   "ipv6 loopback origin is allowed",
			host:   "encoder.local",
			origin: "http://[::1]:8080",
			want:   true,
		},
		{
			name:   "same host with request port is allowed",
			host:   "encoder.local:8080",
			origin: "https://encoder.local",
			want:   true,
		},
		{
			name:   "same host without request port is allowed",
			host:   "encoder.local",
			origin: "https://encoder.local:8443",
			want:   true,
		},
		{
			name:   "private IPv4 origin is allowed",
			host:   "encoder.local",
			origin: "http://192.168.1.20:8080",
			want:   true,
		},
		{
			name:   "private IPv6 origin is allowed",
			host:   "encoder.local",
			origin: "http://[fd00::1]:8080",
			want:   true,
		},
		{
			name:   "external origin is rejected",
			host:   "encoder.local",
			origin: "https://example.com",
			want:   false,
		},
		{
			name:   "invalid origin is rejected",
			host:   "encoder.local",
			origin: "://bad-origin",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			if got := checkOrigin(req); got != tt.want {
				t.Fatalf("checkOrigin() = %t, want %t", got, tt.want)
			}
		})
	}
}
