package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	serverpkg "github.com/oszuidwest/zwfm-encoder/internal/server"
)

func TestSetupRoutesRejectsUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	publicRoutes := map[string]struct{}{
		"/login":       {},
		"/logout":      {},
		"GET /health":  {},
		"GET /ready":   {},
		"/style.css":   {},
		"/icons.js":    {},
		"/favicon.svg": {},
	}
	apiKeyRoutes := map[string]struct{}{
		"POST /api/recordings/start": {},
		"POST /api/recordings/stop":  {},
	}

	type testCase struct {
		name   string
		method string
		path   string
	}
	tests := []testCase{}
	seenPublic := map[string]bool{}
	seenAPIKey := map[string]bool{}
	for _, pattern := range setupRoutePatterns(t) {
		if _, ok := publicRoutes[pattern]; ok {
			seenPublic[pattern] = true
			continue
		}
		if _, ok := apiKeyRoutes[pattern]; ok {
			seenAPIKey[pattern] = true
			continue
		}

		method, path := requestForRoutePattern(pattern)
		tests = append(tests, testCase{
			name:   method + " " + path,
			method: method,
			path:   path,
		})
	}
	assertAllowlistedRoutesRegistered(t, publicRoutes, seenPublic)
	assertAllowlistedRoutesRegistered(t, apiKeyRoutes, seenAPIKey)

	handler := newRouteTestServer(t).SetupRoutes()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tt.method, tt.path, http.NoBody)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				return
			}
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
				t.Fatalf(
					"%s response = %d with Location %q, want 401 or redirect to /login",
					tt.name,
					rec.Code,
					rec.Header().Get("Location"),
				)
			}
		})
	}
}

func TestSetupRoutesAPIKeyAuth(t *testing.T) {
	t.Parallel()

	const apiKey = "route-test-api-key" //nolint:gosec // Test credential for route authentication.
	tests := []struct {
		name         string
		configured   bool
		providedKey  string
		wantStatus   int
		wantErrorMsg string
		wantJSON     bool
	}{
		{
			name:         "no key configured",
			wantStatus:   http.StatusServiceUnavailable,
			wantErrorMsg: "API key not configured",
		},
		{
			name:         "missing header",
			configured:   true,
			wantStatus:   http.StatusUnauthorized,
			wantErrorMsg: "Unauthorized",
		},
		{
			name:         "wrong key",
			configured:   true,
			providedKey:  "wrong-key",
			wantStatus:   http.StatusUnauthorized,
			wantErrorMsg: "Unauthorized",
		},
		{
			name:         "correct key calls handler",
			configured:   true,
			providedKey:  apiKey,
			wantStatus:   http.StatusBadRequest,
			wantErrorMsg: "recorder_id is required",
			wantJSON:     true,
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

			req := httptest.NewRequest(http.MethodPost, "/api/recordings/start", http.NoBody)
			if tt.providedKey != "" {
				req.Header.Set("X-API-Key", tt.providedKey)
			}
			rec := httptest.NewRecorder()
			s.SetupRoutes().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			got := strings.TrimSpace(rec.Body.String())
			if tt.wantJSON {
				got = decodeError(t, rec.Body.Bytes())
			}
			if got != tt.wantErrorMsg {
				t.Fatalf("body = %q, want %q", got, tt.wantErrorMsg)
			}
		})
	}
}

func TestHandleExternalRecordingActionThroughRoute(t *testing.T) {
	t.Parallel()

	const apiKey = "external-recording-test-key" //nolint:gosec // Test credential for route authentication.
	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantErrorMsg string
	}{
		{
			name:         "missing recorder ID",
			path:         "/api/recordings/start",
			wantStatus:   http.StatusBadRequest,
			wantErrorMsg: "recorder_id is required",
		},
		{
			name:         "recording not available",
			path:         "/api/recordings/stop?recorder_id=unknown",
			wantStatus:   http.StatusServiceUnavailable,
			wantErrorMsg: encoder.ErrRecordingNotAvailable.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRouteTestServer(t)
			if err := s.config.SetRecordingAPIKey(apiKey); err != nil {
				t.Fatalf("SetRecordingAPIKey() error = %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, tt.path, http.NoBody)
			req.Header.Set("X-API-Key", apiKey)
			rec := httptest.NewRecorder()
			s.SetupRoutes().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := decodeError(t, rec.Body.Bytes()); got != tt.wantErrorMsg {
				t.Fatalf("error = %q, want %q", got, tt.wantErrorMsg)
			}
		})
	}
}

func TestSecurityHeadersOnPublicResponse(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/login", http.NoBody)
	rec := httptest.NewRecorder()
	newRouteTestServer(t).SetupRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	tests := []struct {
		name  string
		value string
	}{
		{name: "X-Frame-Options", value: "DENY"},
		{name: "X-Content-Type-Options", value: "nosniff"},
		{name: "Referrer-Policy", value: "strict-origin-when-cross-origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rec.Header().Get(tt.name); got != tt.value {
				t.Fatalf("header %s = %q, want %q", tt.name, got, tt.value)
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

// setupRoutePatterns reads every mux registration from server.go so a route
// added to SetupRoutes is covered without editing this test.
func setupRoutePatterns(t *testing.T) []string {
	t.Helper()

	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	var patterns []string
	for _, match := range routeRegistration.FindAllStringSubmatch(string(source), -1) {
		if match[1] == "" {
			t.Fatalf("route registration %q does not use a string literal pattern", match[0])
		}
		patterns = append(patterns, match[1])
	}
	if len(patterns) == 0 {
		t.Fatal("SetupRoutes contains no route registrations")
	}
	return patterns
}

var (
	routeRegistration = regexp.MustCompile(`mux\.Handle(?:Func)?\((?:"([^"]+)")?`)
	routeWildcard     = regexp.MustCompile(`\{[^}]*\}`)
)

func requestForRoutePattern(pattern string) (method, path string) {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		method, path = http.MethodGet, pattern
	}
	return method, routeWildcard.ReplaceAllString(path, "test")
}

func assertAllowlistedRoutesRegistered(
	t *testing.T,
	allowlist map[string]struct{},
	seen map[string]bool,
) {
	t.Helper()

	for pattern := range allowlist {
		if !seen[pattern] {
			t.Errorf("allowlisted route %q is not registered by SetupRoutes", pattern)
		}
	}
}
