package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionManagerCreateReturnsValidToken(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	token := sm.Create()
	if token == "" {
		t.Fatal("Create() returned an empty token")
	}
	if !sm.Validate(token) {
		t.Fatal("created token did not validate")
	}
}

// invalidTokenCases covers tokens that no validator should ever accept.
var invalidTokenCases = []struct {
	name  string
	token string
}{
	{name: "unknown token is rejected", token: "unknown-token"},
	{name: "empty token is rejected", token: ""},
}

func TestSessionManagerValidateRejectsInvalidTokens(t *testing.T) {
	t.Parallel()

	for _, tt := range invalidTokenCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := NewSessionManager()
			if sm.Validate(tt.token) {
				t.Fatalf("Validate(%q) = true, want false", tt.token)
			}
		})
	}
}

func TestSessionManagerDeleteRemovesToken(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	token := sm.Create()
	sm.Delete(token)

	if sm.Validate(token) {
		t.Fatal("deleted token validated, want false")
	}
}

func TestValidateRejectsAndRemovesExpiredTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		seed     func(*SessionManager, string)
		validate func(*SessionManager, string) bool
		present  func(*SessionManager, string) bool
	}{
		{
			name: "session",
			seed: func(sm *SessionManager, tok string) {
				sm.sessions[tok] = &session{expiresAt: time.Now().Add(-time.Minute)}
			},
			validate: (*SessionManager).Validate,
			present:  func(sm *SessionManager, tok string) bool { _, ok := sm.sessions[tok]; return ok },
		},
		{
			name: "csrf token",
			seed: func(sm *SessionManager, tok string) {
				sm.csrfTokens[tok] = &csrfToken{expiresAt: time.Now().Add(-time.Minute)}
			},
			validate: (*SessionManager).ValidateCSRFToken,
			present:  func(sm *SessionManager, tok string) bool { _, ok := sm.csrfTokens[tok]; return ok },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := NewSessionManager()
			const token = "expired-token"
			tt.seed(sm, token)

			if tt.validate(sm, token) {
				t.Fatalf("validate(expired %s) = true, want false", tt.name)
			}
			if tt.present(sm, token) {
				t.Fatalf("expired %s still present after validation", tt.name)
			}
		})
	}
}

func TestSessionManagerCreateCSRFTokenValidatesOnce(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	token := sm.CreateCSRFToken()
	if token == "" {
		t.Fatal("CreateCSRFToken() returned an empty token")
	}

	if !sm.ValidateCSRFToken(token) {
		t.Fatal("created CSRF token did not validate")
	}
	if sm.ValidateCSRFToken(token) {
		t.Fatal("CSRF token validated twice, want single use")
	}
}

func TestSessionManagerValidateCSRFTokenRejectsInvalidTokens(t *testing.T) {
	t.Parallel()

	for _, tt := range invalidTokenCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := NewSessionManager()
			if sm.ValidateCSRFToken(tt.token) {
				t.Fatalf("ValidateCSRFToken(%q) = true, want false", tt.token)
			}
		})
	}
}

func TestSessionManagerAuthMiddleware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setupRequest func(*SessionManager, *http.Request)
		wantStatus   int
		wantLocation string
		wantCalled   bool
	}{
		{
			name:         "missing cookie redirects to login",
			wantStatus:   http.StatusFound,
			wantLocation: "/login",
		},
		{
			name: "unknown session cookie redirects to login",
			setupRequest: func(_ *SessionManager, r *http.Request) {
				r.AddCookie(requestSessionCookie("unknown-token"))
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/login",
		},
		{
			name: "valid session cookie calls next handler",
			setupRequest: func(sm *SessionManager, r *http.Request) {
				r.AddCookie(requestSessionCookie(sm.Create()))
			},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := NewSessionManager()
			req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
			if tt.setupRequest != nil {
				tt.setupRequest(sm, req)
			}

			called := false
			handler := sm.AuthMiddleware()(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})

			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", got, tt.wantLocation)
			}
			if called != tt.wantCalled {
				t.Fatalf("next handler called = %t, want %t", called, tt.wantCalled)
			}
		})
	}
}

func TestSetSessionCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(*http.Request)
		wantSecure bool
	}{
		{
			name: "plain HTTP is not secure",
		},
		{
			name: "TLS request is secure",
			setup: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{}
			},
			wantSecure: true,
		},
		{
			name: "forwarded HTTPS request is secure",
			setup: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			wantSecure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			if tt.setup != nil {
				tt.setup(req)
			}
			rec := httptest.NewRecorder()

			setSessionCookie(rec, req, "session-token", 123)

			cookie := requireOneSessionCookie(t, rec)
			if cookie.Value != "session-token" {
				t.Fatalf("cookie value = %q, want session-token", cookie.Value)
			}
			if cookie.Path != "/" {
				t.Fatalf("cookie path = %q, want /", cookie.Path)
			}
			if cookie.MaxAge != 123 {
				t.Fatalf("cookie MaxAge = %d, want 123", cookie.MaxAge)
			}
			if !cookie.HttpOnly {
				t.Fatal("cookie HttpOnly = false, want true")
			}
			if cookie.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie SameSite = %v, want %v", cookie.SameSite, http.SameSiteStrictMode)
			}
			if cookie.Secure != tt.wantSecure {
				t.Fatalf("cookie Secure = %t, want %t", cookie.Secure, tt.wantSecure)
			}
		})
	}
}

func TestSessionManagerLogin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		username   string
		password   string
		wantOK     bool
		wantCookie bool
	}{
		{
			name:     "wrong username is rejected without cookie",
			username: "operator",
			password: "secret",
		},
		{
			name:     "wrong password is rejected without cookie",
			username: "admin",
			password: "incorrect",
		},
		{
			name:       "correct credentials create session cookie",
			username:   "admin",
			password:   "secret",
			wantOK:     true,
			wantCookie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := NewSessionManager()
			req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
			rec := httptest.NewRecorder()

			ok := sm.Login(rec, req, tt.username, tt.password, "admin", "secret")
			if ok != tt.wantOK {
				t.Fatalf("Login() = %t, want %t", ok, tt.wantOK)
			}

			cookies := rec.Result().Cookies()
			if gotCookie := len(cookies) > 0; gotCookie != tt.wantCookie {
				t.Fatalf("cookie set = %t, want %t", gotCookie, tt.wantCookie)
			}
			if !tt.wantCookie {
				return
			}

			cookie := cookies[0]
			if cookie.Name != sessionCookieName {
				t.Fatalf("cookie name = %q, want %q", cookie.Name, sessionCookieName)
			}
			if !sm.Validate(cookie.Value) {
				t.Fatal("session cookie value did not validate")
			}
		})
	}
}

func TestSessionManagerLogoutClearsCookieAndDeletesSession(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	token := sm.Create()
	req := httptest.NewRequest(http.MethodGet, "/logout", http.NoBody)
	req.AddCookie(requestSessionCookie(token))
	rec := httptest.NewRecorder()

	sm.Logout(rec, req)

	if sm.Validate(token) {
		t.Fatal("session validates after Logout, want deleted")
	}

	cookie := requireOneSessionCookie(t, rec)
	if cookie.MaxAge != -1 {
		t.Fatalf("cookie MaxAge = %d, want -1", cookie.MaxAge)
	}
}

func requestSessionCookie(value string) *http.Cookie {
	// Requests only carry the cookie name and value; other attributes are response-only.
	//nolint:gosec // G124: request-side test cookie; Secure/HttpOnly/SameSite are response-only.
	return &http.Cookie{Name: sessionCookieName, Value: value}
}

// requireOneSessionCookie asserts exactly one session cookie was set and returns it.
func requireOneSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("len(cookies) = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, sessionCookieName)
	}
	return cookie
}
