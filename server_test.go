package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	serverpkg "github.com/oszuidwest/zwfm-encoder/internal/server"
)

func TestSetupRoutesRejectsUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	// Routes that skip session auth; the API-key routes are covered by
	// TestSetupRoutesAPIKeyAuth.
	unprotected := map[string]bool{
		"/login":                     true,
		"GET /health":                true,
		"GET /ready":                 true,
		"/style.css":                 true,
		"/icons.js":                  true,
		"/favicon.svg":               true,
		"POST /api/recordings/start": true,
		"POST /api/recordings/stop":  true,
	}

	handler := newRouteTestServer(t).SetupRoutes()
	for _, pattern := range setupRoutePatterns(t) {
		if unprotected[pattern] {
			delete(unprotected, pattern)
			continue
		}

		method, path, found := strings.Cut(pattern, " ")
		if !found {
			method, path = http.MethodGet, pattern
		}
		t.Run(method+" "+path, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(method, path, http.NoBody)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// A request that falls through to the protected "/" must not pass
			// for its own route.
			if req.Pattern != pattern {
				t.Fatalf("request %s %s reached pattern %q, want %q", method, path, req.Pattern, pattern)
			}

			// Pages redirect to the login form; API requests and mutations answer
			// 401 so the page's fetch does not follow the redirect and report success.
			if method != http.MethodGet || strings.HasPrefix(path, "/api/") {
				assertStatus(t, rec, http.StatusUnauthorized)
				return
			}
			assertStatus(t, rec, http.StatusFound)
			if location := rec.Header().Get("Location"); location != "/login" {
				t.Fatalf("Location = %q, want /login", location)
			}
		})
	}
	for pattern := range unprotected {
		t.Errorf("allowlisted route %q is not registered by SetupRoutes", pattern)
	}
}

func TestSetupRoutesAPIKeyAuth(t *testing.T) {
	t.Parallel()

	const apiKey = "route-test-api-key" //nolint:gosec // Test credential for route authentication.
	tests := []struct {
		name       string
		configured bool
		key        string
		path       string
		wantStatus int
		wantBody   string
		wantJSON   bool
	}{
		{
			name:       "no key configured",
			path:       "/api/recordings/start",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "API key not configured",
		},
		{
			name:       "missing header",
			configured: true,
			path:       "/api/recordings/start",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "Unauthorized",
		},
		{
			name:       "wrong key",
			configured: true,
			key:        strings.Repeat("x", len(apiKey)),
			path:       "/api/recordings/stop",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "Unauthorized",
		},
		{
			name:       "correct key reaches handler",
			configured: true,
			key:        apiKey,
			path:       "/api/recordings/start",
			wantStatus: http.StatusBadRequest,
			wantBody:   "recorder_id is required",
			wantJSON:   true,
		},
		{
			name:       "recording not available",
			configured: true,
			key:        apiKey,
			path:       "/api/recordings/stop?recorder_id=unknown",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   encoder.ErrRecordingNotAvailable.Error(),
			wantJSON:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRouteTestServer(t)
			if tt.configured {
				if err := s.config.SetRecordingAPIKey(apiKey); err != nil {
					t.Fatalf("SetRecordingAPIKey() error = %v", err)
				}
			}

			req := httptest.NewRequest(http.MethodPost, tt.path, http.NoBody)
			if tt.key != "" {
				req.Header.Set("X-API-Key", tt.key)
			}
			rec := httptest.NewRecorder()
			s.SetupRoutes().ServeHTTP(rec, req)

			assertStatus(t, rec, tt.wantStatus)
			if tt.wantJSON {
				assertErrorEqual(t, rec, tt.wantBody)
			} else if got := strings.TrimSpace(rec.Body.String()); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestSecurityHeadersOnPublicResponse(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/login", http.NoBody)
	rec := httptest.NewRecorder()
	newRouteTestServer(t).SetupRoutes().ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusOK)

	for name, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rec.Result().Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestConnectionTestsOutlastWriteTimeout(t *testing.T) {
	// Keep this test serial: the swapped http.DefaultTransport is process-wide.

	// Every endpoint answers only after the web server's write timeout.
	const writeTimeout = 100 * time.Millisecond
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * writeTimeout)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(slow.Close)
	// The Graph hosts are fixed, so send the email test through the slow server as a proxy.
	proxy, err := url.Parse(slow.URL)
	if err != nil {
		t.Fatal(err)
	}
	defaultTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = defaultTransport })
	http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	zabbix, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zabbix.Close() })
	go func() {
		for {
			conn, err := zabbix.Accept()
			if err != nil {
				return
			}
			time.AfterFunc(3*writeTimeout, func() { _ = conn.Close() })
		}
	}()
	zabbixPort := strconv.Itoa(zabbix.Addr().(*net.TCPAddr).Port)

	const graphID = "12345678-1234-1234-1234-123456789abc"
	tests := []struct{ path, body, wantError string }{
		{
			path:      "/api/recorders/test-s3",
			body:      `{"s3_endpoint":"` + slow.URL + `","s3_bucket":"b","s3_access_key_id":"k","s3_secret_access_key":"s"}`,
			wantError: "upload test file",
		},
		{
			path:      "/api/notifications/test/webhook",
			body:      `{"webhook_url":"` + slow.URL + `"}`,
			wantError: "webhook returned status 403",
		},
		{
			path:      "/api/notifications/test/email",
			body:      `{"graph_tenant_id":"` + graphID + `","graph_client_id":"` + graphID + `","graph_client_secret":"s","graph_from_address":"a@example.com","graph_recipients":"b@example.com"}`,
			wantError: "send email",
		},
		{
			path:      "/api/notifications/test/zabbix",
			body:      `{"zabbix_server":"127.0.0.1","zabbix_port":` + zabbixPort + `,"zabbix_host":"h","zabbix_silence_key":"k"}`,
			wantError: "read zabbix reply header",
		},
	}
	for _, tt := range tests {
		t.Run("POST "+tt.path, func(t *testing.T) {
			t.Parallel()

			s := newRouteTestServer(t)
			ts := httptest.NewUnstartedServer(s.SetupRoutes())
			ts.Config.WriteTimeout = writeTimeout
			ts.Start()
			t.Cleanup(ts.Close)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: "encoder_session", Value: s.sessions.Create()}) //nolint:gosec // G124: request cookie only.
			req.Header.Set("X-CSRF-Token", s.sessions.CSRFToken(req))
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("test result lost after the write timeout: %v", err)
			}
			defer func() { _ = res.Body.Close() }()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeError(t, body); res.StatusCode != http.StatusBadGateway || !strings.Contains(got, tt.wantError) {
				t.Fatalf("status = %d, error = %q, want 502 containing %q", res.StatusCode, got, tt.wantError)
			}
		})
	}
}

func newRouteTestServer(t *testing.T) *Server {
	t.Helper()

	s := freshServer(t)
	s.encoder = &encoder.Encoder{}
	s.sessions = serverpkg.NewSessionManager()
	return s
}

// setupRoutePatterns parses the package's non-test files and returns every
// Handle/HandleFunc pattern, so a new route is covered without editing this test.
func setupRoutePatterns(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("%s: route registration %s does not use a string literal pattern", name, selector.Sel.Name)
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: route pattern %s: %v", name, literal.Value, err)
			}
			patterns = append(patterns, pattern)
			return true
		})
	}
	if len(patterns) == 0 {
		t.Fatal("no route registrations found")
	}
	return patterns
}

func TestWebSocketWriterStopsAfterLogout(t *testing.T) {
	t.Parallel()

	s := newRouteTestServer(t)
	session := s.sessions.Create()
	send := make(chan any, 1)
	t.Cleanup(func() { close(send) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := serverpkg.UpgradeConnection(w, r); err == nil {
			s.runWebSocketWriter(conn, r, send)
		}
	}))
	t.Cleanup(ts.Close)
	header := http.Header{"Cookie": {"encoder_session=" + session}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	send <- rawWSMessage(`"live"`)
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != `"live"` {
		t.Fatalf("message during session = %q, %v", msg, err)
	}
	s.sessions.Delete(session)
	send <- rawWSMessage(`"after logout"`)
	var closed *websocket.CloseError
	if _, msg, err := conn.ReadMessage(); !errors.As(err, &closed) {
		t.Fatalf("after logout got %q, %v; want closed connection", msg, err)
	}
}

func TestWebSocketWriterGivesUpOnStalledClient(t *testing.T) {
	// Keep this test serial: the shortened wsWriteTimeout is package-wide.
	old := wsWriteTimeout
	wsWriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { wsWriteTimeout = old })

	s := newRouteTestServer(t)
	session := s.sessions.Create()
	send := make(chan any, 1)
	t.Cleanup(func() { close(send) })
	returned := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := serverpkg.UpgradeConnection(w, r); err == nil {
			s.runWebSocketWriter(conn, r, send)
			close(returned)
		}
	}))
	t.Cleanup(ts.Close)
	header := http.Header{"Cookie": {"encoder_session=" + session}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })

	// The client never reads, so a message far beyond the socket buffers blocks.
	send <- make(rawWSMessage, 64<<20)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("writer still blocked on a client that stopped reading")
	}
}
