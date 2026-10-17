package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
)

func TestAllSessionMutationRoutesRequireCSRF(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	handler := s.SetupRoutes()
	for _, pattern := range setupRoutePatterns(t) {
		method, path, found := strings.Cut(pattern, " ")
		if !found || method == http.MethodGet || strings.HasPrefix(path, "/api/recordings/") {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			req := httptest.NewRequest(method, path, http.NoBody)
			req.AddCookie(&http.Cookie{Name: "encoder_session", Value: s.sessions.Create()}) //nolint:gosec // G124: request cookie only; response flags are tested below.
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if req.Pattern != pattern || rec.Code != http.StatusForbidden {
				t.Fatalf("%s matched %q: status %d, want CSRF denial", pattern, req.Pattern, rec.Code)
			}
		})
	}
}

func TestAdminHTTPSLoginMutationAndLogout(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	// A busy anonymous token pool must not block a legitimate login.
	for range 4096 {
		s.sessions.CreateCSRFToken()
	}
	ts := httptest.NewTLSServer(s.SetupRoutes())
	defer ts.Close()
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = get.Body.Close() }()
	page := readAdminBody(t, get)
	csrf := adminToken(t, page, `name="csrf_token" value="([a-f0-9]+)"`)
	form := url.Values{"username": {config.DefaultWebUsername}, "password": {config.DefaultWebPassword}, "csrf_token": {csrf}}
	login, err := client.PostForm(ts.URL+"/login", form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = login.Body.Close() }()
	_ = readAdminBody(t, login)
	if login.StatusCode != http.StatusFound || len(login.Cookies()) != 1 {
		t.Fatalf("login = %d, cookies %v", login.StatusCode, login.Cookies())
	}
	cookie := login.Cookies()[0]
	assertAdminCookieFlags(t, cookie)
	request := func(method, path, token, origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	index := request(http.MethodGet, "/", "", "")
	defer func() { _ = index.Body.Close() }()
	body := readAdminBody(t, index)
	token := adminToken(t, body, `name="csrf-token" content="([a-f0-9]+)"`)
	if index.Header.Get("Cache-Control") != "no-store" || token == csrf || token == cookie.Value {
		t.Fatal("page token must be session-bound, distinct and uncacheable")
	}
	for _, tc := range []struct{ name, token, origin string }{
		{name: "no token"},
		{name: "login token", token: csrf},
		{name: "foreign origin", token: token, origin: "https://attacker.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := request(http.MethodPost, "/api/recording/regenerate-key", tc.token, tc.origin)
			defer func() { _ = res.Body.Close() }()
			_ = readAdminBody(t, res)
			if res.StatusCode != http.StatusForbidden || s.config.Snapshot().RecordingAPIKey != "" {
				t.Fatal("forged request changed the config")
			}
		})
	}
	res := request(http.MethodPost, "/api/recording/regenerate-key", token, ts.URL)
	defer func() { _ = res.Body.Close() }()
	_ = readAdminBody(t, res)
	if res.StatusCode != http.StatusOK || s.config.Snapshot().RecordingAPIKey == "" {
		t.Fatalf("valid CSRF request = %d; config unchanged", res.StatusCode)
	}
	configRes := request(http.MethodGet, "/api/config", "", "")
	defer func() { _ = configRes.Body.Close() }()
	configBody := readAdminBody(t, configRes)
	if strings.Contains(configBody, s.config.Snapshot().WebPasswordHash) || strings.Contains(configBody, `"password_hash"`) {
		t.Fatal("API leaked the admin password hash")
	}
	checkAdminLogout(t, s, request, token, cookie)
}

func assertAdminCookieFlags(t *testing.T, cookie *http.Cookie) {
	t.Helper()
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("HTTPS session cookie flags weakened")
	}
}

func checkAdminLogout(t *testing.T, s *Server, request func(string, string, string, string) *http.Response, token string, cookie *http.Cookie) {
	t.Helper()
	getLogout := request(http.MethodGet, "/logout", "", "")
	defer func() { _ = getLogout.Body.Close() }()
	_ = readAdminBody(t, getLogout)
	if !s.sessions.Validate(cookie.Value) {
		t.Fatal("GET logout mutated the session")
	}
	missing := request(http.MethodPost, "/logout", "", "")
	defer func() { _ = missing.Body.Close() }()
	_ = readAdminBody(t, missing)
	if missing.StatusCode != http.StatusForbidden || !s.sessions.Validate(cookie.Value) {
		t.Fatal("logout without CSRF was accepted")
	}
	logout := request(http.MethodPost, "/logout", token, "")
	defer func() { _ = logout.Body.Close() }()
	_ = readAdminBody(t, logout)
	if logout.StatusCode != http.StatusNoContent || s.sessions.Validate(cookie.Value) || len(logout.Cookies()) != 1 || logout.Cookies()[0].MaxAge >= 0 {
		t.Fatal("valid logout did not revoke session/cookie")
	}
	revoked := request(http.MethodPost, "/api/recording/regenerate-key", token, "")
	defer func() { _ = revoked.Body.Close() }()
	_ = readAdminBody(t, revoked)
	if revoked.StatusCode != http.StatusFound {
		t.Fatal("revoked session still reaches a mutation")
	}
}

func TestLoginThrottleAndCrossOriginRejection(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	handler := s.SetupRoutes()
	for attempt := range 6 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))
		token := adminToken(t, rec.Body.String(), `name="csrf_token" value="([a-f0-9]+)"`)
		password := "wrong"
		if attempt == 5 {
			password = config.DefaultWebPassword
		}
		form := url.Values{"username": {config.DefaultWebUsername}, "password": {password}, "csrf_token": {token}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if attempt < 5 {
			if rec.Code != http.StatusOK {
				t.Fatalf("attempt %d = %d", attempt, rec.Code)
			}
		} else if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
			t.Fatalf("throttle = %d, %v", rec.Code, rec.Header())
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatal("failed/throttled login set a session")
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d", rec.Code)
	}
}

func TestLoginRejectsMalformedOrQueryCredentials(t *testing.T) {
	t.Parallel()
	s := newRouteTestServer(t)
	handler := s.SetupRoutes()
	for _, tc := range []struct {
		name, query, body string
		wantStatus        int
	}{
		{name: "query credentials", query: "?username=admin&password=encoder", wantStatus: http.StatusOK},
		{name: "invalid encoding", body: "username=admin&password=%xx", wantStatus: http.StatusBadRequest},
		{name: "oversized body", body: "username=admin&password=" + strings.Repeat("p", 4097), wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if tc.query != "" {
				page := httptest.NewRecorder()
				handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))
				token := adminToken(t, page.Body.String(), `name="csrf_token" value="([a-f0-9]+)"`)
				body = url.Values{"csrf_token": {token}}.Encode()
			}
			req := httptest.NewRequest(http.MethodPost, "/login"+tc.query, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus || len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
				t.Fatalf("invalid form = %d, cookies %v", rec.Code, rec.Result().Cookies())
			}
			if tc.query != "" && !strings.Contains(rec.Body.String(), `id="login-error"`) {
				t.Fatal("query-only credentials must reach a failed-login page")
			}
		})
	}
}

func TestServerStartUsesConfiguredBind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ bind, want string }{{"127.0.0.1", "127.0.0.1:0"}, {"::1", "[::1]:0"}} {
		t.Run(tc.bind, func(t *testing.T) {
			s := newRouteTestServer(t)
			// Port zero is test-only, so the OS chooses an unused loopback port.
			s.config.System.Port = 0
			s.config.System.BindAddress = tc.bind
			s.version = &VersionChecker{stopCh: make(chan struct{})}
			s.Start()
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := s.Stop(ctx); err != nil {
					t.Error(err)
				}
			}()
			if s.httpServer.Addr != tc.want {
				t.Fatalf("server listen address = %q, want %q", s.httpServer.Addr, tc.want)
			}
		})
	}
}

func adminToken(t *testing.T, page, pattern string) string {
	t.Helper()
	match := regexp.MustCompile(pattern).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("page omitted its CSRF token")
	}
	return match[1]
}

func readAdminBody(t *testing.T, res *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
