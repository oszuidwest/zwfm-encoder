package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestSessionMutationsRejectMissingOrUnboundCSRF(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			sm := NewSessionManager()
			for _, token := range []string{"", "wrong", sm.CreateCSRFToken()} {
				req := httptest.NewRequest(method, "/api/settings", http.NoBody)
				req.AddCookie(requestSessionCookie(sm.Create()))
				req.Header.Set("X-CSRF-Token", token)
				rec := httptest.NewRecorder()
				sm.AuthMiddleware()(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("mutation with unbound token = %d, want 403", rec.Code)
				}
			}
		})
	}
}

func TestSessionCSRFMatchesOnlyItsLiveSession(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	sessionToken := sm.Create()
	req.AddCookie(requestSessionCookie(sessionToken))
	csrf := sm.CSRFToken(req)
	if csrf == "" || csrf == sessionToken {
		t.Fatal("CSRF must be distinct from the HttpOnly session credential")
	}
	other := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	other.AddCookie(requestSessionCookie(sm.Create()))
	if csrf == sm.CSRFToken(other) {
		t.Fatal("CSRF tokens must be unique per session")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for range 2 {
			req := httptest.NewRequest(method, "/api/settings", http.NoBody)
			req.AddCookie(requestSessionCookie(sessionToken))
			if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
				req.Header.Set("X-CSRF-Token", csrf)
			}
			rec := httptest.NewRecorder()
			sm.AuthMiddleware()(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(rec, req)
			if rec.Code != http.StatusNoContent || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("valid %s = %d, %v", method, rec.Code, rec.Header())
			}
		}
	}
	other.Method = http.MethodPost
	other.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	sm.AuthMiddleware()(func(http.ResponseWriter, *http.Request) { t.Error("another session accepted the token") })(rec, other)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-session CSRF = %d", rec.Code)
	}
	sm.Delete(sessionToken)
	if sm.CSRFToken(req) != "" {
		t.Fatal("deleted session retained CSRF")
	}
}

func TestHashedLogin(t *testing.T) {
	t.Parallel()
	hash, err := util.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, password, plaintext, hash string
		want                                  bool
	}{
		{name: "correct", user: "admin", password: "correct horse", hash: hash, want: true},
		{name: "wrong user", user: "other", password: "correct horse", hash: hash},
		{name: "wrong password", user: "admin", password: "wrong", hash: hash},
		{name: "hash is not a password", user: "admin", password: hash, hash: hash},
		{name: "malformed does not fall back", user: "admin", password: "plaintext", plaintext: "plaintext", hash: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewSessionManager()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
			if got := sm.Login(rec, req, tc.user, tc.password, "admin", tc.plaintext, tc.hash); got != tc.want {
				t.Fatalf("Login = %t, want %t", got, tc.want)
			}
			if (len(rec.Result().Cookies()) > 0) != tc.want {
				t.Fatal("failed login set a session cookie")
			}
		})
	}
}

func TestSessionCSRFExpiresWithSession(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sm := NewSessionManager()
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.AddCookie(requestSessionCookie(sm.Create()))
		if sm.CSRFToken(req) == "" {
			t.Fatal("new session has no CSRF token")
		}
		time.Sleep(sessionDuration)
		if sm.CSRFToken(req) != "" {
			t.Fatal("expired session kept its CSRF token")
		}
	})
}

func TestLoginRateLimitsByIPAndUsername(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sm := NewSessionManager()
		for i := range 5 {
			req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
			req.RemoteAddr = fmt.Sprintf("192.0.2.1:%d", 1000+i)
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
			if !sm.AllowLogin(req, fmt.Sprintf("user%d", i)) {
				t.Fatal("early IP throttle")
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
		req.RemoteAddr = "[::ffff:192.0.2.1]:9999"
		if sm.AllowLogin(req, "another") {
			t.Fatal("port, forwarded header or IPv4-mapped address bypassed IP limit")
		}
		time.Sleep(time.Minute)
		if !sm.AllowLogin(req, "another") {
			t.Fatal("IP limit did not expire")
		}
		sm = NewSessionManager()
		for i := range 10 {
			req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
			if !sm.AllowLogin(req, "admin") {
				t.Fatal("early username throttle")
			}
		}
		req.RemoteAddr = "198.51.100.1:1234"
		if sm.AllowLogin(req, "admin") {
			t.Fatal("changing IP bypassed username limit")
		}
		if !sm.AllowLogin(req, "other") {
			t.Fatal("one username blocked every account")
		}
		time.Sleep(time.Minute)
		if !sm.AllowLogin(req, "admin") {
			t.Fatal("username limit did not expire")
		}
	})
}

func TestConcurrentLoginReservationsAreBounded(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
			req.RemoteAddr = "192.0.2.1:1234"
			if sm.AllowLogin(req, "admin") {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("concurrent allowed attempts = %d, want 5", got)
	}
}

func TestLoginRateStateIsBoundedAndRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sm := NewSessionManager()
		for i := range maxLoginBuckets {
			sm.loginAttempts[fmt.Sprint(i)] = loginAttempt{count: 1, expiresAt: time.Now().Add(time.Minute)}
		}
		req := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
		if sm.AllowLogin(req, strings.Repeat("u", 100)) || len(sm.loginAttempts) != maxLoginBuckets {
			t.Fatal("full limiter allocated new entries or failed open")
		}
		time.Sleep(time.Minute)
		if !sm.AllowLogin(req, "admin") || len(sm.loginAttempts) != 2 {
			t.Fatal("expired limiter state was not reclaimed")
		}
	})
}

func TestLoginCSRFPoolIsBoundedAndRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sm := NewSessionManager()
		for i := range 4096 {
			sm.csrfTokens[fmt.Sprint(i)] = &csrfToken{expiresAt: time.Now().Add(csrfTokenDuration)}
		}
		if sm.CreateCSRFToken() != "" || len(sm.csrfTokens) != 4096 {
			t.Fatal("full login token pool failed open")
		}
		time.Sleep(csrfTokenDuration)
		if sm.CreateCSRFToken() == "" || len(sm.csrfTokens) != 1 {
			t.Fatal("expired login tokens were not reclaimed")
		}
	})
}
