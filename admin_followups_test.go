package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	serverpkg "github.com/oszuidwest/zwfm-encoder/internal/server"
)

func TestMalformedTrustedForwardingReportsBadRequestNotThrottle(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	s.config.System.TrustedProxies = []string{"127.0.0.1/32"}
	handler := s.SetupRoutes()
	for _, address := range []string{"unknown", "198.51.100.1:1234", "[2001:db8::1]", "bad"} {
		for range 4 {
			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))
			token := adminToken(t, page.Body.String(), `name="csrf_token" value="([a-f0-9]+)"`)
			form := url.Values{"username": {config.DefaultWebUsername}, "password": {"wrong"}, "csrf_token": {token}}
			r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("X-Forwarded-For", address)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Retry-After") != "" || !strings.Contains(rec.Body.String(), "trusted proxy") {
				t.Fatalf("bad forwarded address %q: status=%d body=%s", address, rec.Code, rec.Body.String())
			}
		}
	}
	// Sixteen malformed attempts must not exhaust either real-client budget.
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))
	token := adminToken(t, page.Body.String(), `name="csrf_token" value="([a-f0-9]+)"`)
	form := url.Values{"username": {config.DefaultWebUsername}, "password": {config.DefaultWebPassword}, "csrf_token": {token}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusFound {
		t.Fatalf("valid login after bad forwarding: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthenticatedWebSocketRejectsDifferentPortAndAllowsSameOrigin(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	// Exercise the actual authenticated upgrader without starting the
	// application-lifetime status broadcaster or a real audio encoder.
	endpoint := httptest.NewServer(s.sessions.AuthMiddleware()(func(w http.ResponseWriter, r *http.Request) {
		conn, err := serverpkg.UpgradeConnection(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("connected"))
	}))
	defer endpoint.Close()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	for _, tt := range []struct {
		name, origin string
		allowed      bool
	}{
		{"same origin", endpoint.URL, true},
		{"same loopback host different port", "http://127.0.0.1:1", false},
		{"unrelated private IP", "http://192.168.1.1:8080", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{"Origin": {tt.origin}, "Cookie": {"encoder_session=" + s.sessions.Create()}}
			conn, response, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(endpoint.URL, "http")+"/ws", header)
			if conn != nil {
				if tt.allowed {
					if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					_, data, err := conn.ReadMessage()
					if err != nil || string(data) != "connected" {
						t.Fatalf("authenticated socket not usable: %q %v", data, err)
					}
				}
				_ = conn.Close()
			}
			if response != nil {
				_ = response.Body.Close()
			}
			if (err == nil) != tt.allowed {
				t.Fatalf("handshake allowed=%t err=%v", tt.allowed, err)
			}
			if !tt.allowed && (response == nil || response.StatusCode != http.StatusForbidden) {
				t.Fatalf("expected forbidden handshake, got %+v", response)
			}
		})
	}
}
