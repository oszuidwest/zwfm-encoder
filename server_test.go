package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	serverpkg "github.com/oszuidwest/zwfm-encoder/internal/server"
)

func TestSetupRoutesRejectsUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	// Routes that skip session auth; the API-key routes are covered by
	// TestSetupRoutesAPIKeyAuth.
	unprotected := map[string]bool{
		"/login":                     true,
		"/logout":                    true,
		"GET /health":                true,
		"GET /ready":                 true,
		"/style.css":                 true,
		"/icons.js":                  true,
		"/favicon.svg":               true,
		"POST /api/recordings/start": true,
		"POST /api/recordings/stop":  true,
	}

	patterns := setupRoutePatterns(t)
	// probe resolves each request to the pattern it reaches, so a request that
	// falls through to the protected "/" cannot pass for its own route.
	probe := http.NewServeMux()
	for _, pattern := range patterns {
		probe.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}

	handler := newRouteTestServer(t).SetupRoutes()
	for _, pattern := range patterns {
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
			if _, got := probe.Handler(req); got != pattern {
				t.Fatalf("request %s %s reaches pattern %q, want %q", method, path, got, pattern)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

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
			key:        "wrong-key",
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
		if got := rec.Header().Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func newRouteTestServer(t *testing.T) *Server {
	t.Helper()

	s := freshServer(t)
	s.encoder = &encoder.Encoder{}
	s.sessions = serverpkg.NewSessionManager()
	return s
}

// setupRoutePatterns parses server.go and returns every Handle/HandleFunc
// pattern, so a route added to SetupRoutes is covered without editing this test.
func setupRoutePatterns(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var patterns []string
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
			t.Fatalf("route registration %s does not use a string literal pattern", selector.Sel.Name)
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatalf("route pattern %s: %v", literal.Value, err)
		}
		patterns = append(patterns, pattern)
		return true
	})
	if len(patterns) == 0 {
		t.Fatal("server.go contains no route registrations")
	}
	return patterns
}
