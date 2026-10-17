// Package server provides the HTTP server and WebSocket handler for the web interface.
package server

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const (
	sessionCookieName = "encoder_session"
	sessionDuration   = 24 * time.Hour
	csrfTokenDuration = 10 * time.Minute
)

// A session represents an authenticated user session.
type session struct {
	expiresAt time.Time
	csrf      string
}

// A csrfToken represents a CSRF token with expiration.
type csrfToken struct {
	expiresAt time.Time
}

// SessionManager manages user authentication sessions and CSRF tokens.
type SessionManager struct {
	sessions      map[string]*session
	csrfTokens    map[string]*csrfToken
	loginAttempts map[string]loginAttempt
	mu            sync.RWMutex
}

// NewSessionManager creates a new session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:      make(map[string]*session),
		csrfTokens:    make(map[string]*csrfToken),
		loginAttempts: make(map[string]loginAttempt),
	}
}

// generateToken returns a cryptographically secure random token.
func generateToken() string {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Create creates a new session and returns the token.
func (sm *SessionManager) Create() string {
	token := generateToken()
	csrf := generateToken()
	if token == "" || csrf == "" {
		return ""
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()

	// Sweep expired sessions here: expired tokens are otherwise only removed
	// when the same token is presented again, so the map would slowly grow on
	// a process that runs for months. Login is rare, making this cheap.
	maps.DeleteFunc(sm.sessions, func(_ string, s *session) bool {
		return now.After(s.expiresAt)
	})

	sm.sessions[token] = &session{
		expiresAt: now.Add(sessionDuration),
		csrf:      csrf,
	}
	return token
}

// Validate reports whether a session token is valid. The common case (a live
// session) takes only a read lock, since Validate runs on every authenticated
// request; the write lock is needed only to delete an expired entry.
func (sm *SessionManager) Validate(token string) bool {
	if token == "" {
		return false
	}

	now := time.Now()

	sm.mu.RLock()
	sess, exists := sm.sessions[token]
	sm.mu.RUnlock()

	if !exists {
		return false
	}
	if now.After(sess.expiresAt) {
		sm.Delete(token)
		return false
	}
	return true
}

// Delete removes a session token.
func (sm *SessionManager) Delete(token string) {
	if token == "" {
		return
	}
	sm.mu.Lock()
	delete(sm.sessions, token)
	sm.mu.Unlock()
}

// AuthMiddleware returns middleware that requires a valid session cookie.
// Unauthenticated requests are redirected to /login.
func (sm *SessionManager) AuthMiddleware() func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if csrf := sm.CSRFToken(r); csrf != "" {
				if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
					if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
						// JSON so the page's request helper can show why: usually a tab from an older session.
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"error":"Invalid CSRF token; reload this page"}`))
						return
					}
				}
				w.Header().Set("Cache-Control", "no-store")
				next(w, r)
				return
			}

			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// fetch would follow a redirect to the login page and report success.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"Session expired; reload this page"}`))
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}
}

// CSRFToken returns the request's session-bound token, or empty without a live session.
func (sm *SessionManager) CSRFToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	sess := sm.sessions[cookie.Value]
	if sess == nil || !time.Now().Before(sess.expiresAt) {
		return ""
	}
	return sess.csrf
}

// setSessionCookie sets or clears the session cookie.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	//nolint:gosec // G124: Secure depends on request TLS/proxy state so local HTTP deployments keep working.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}

// hashSlot and hashWaiters bound password-hash verification across all logins.
var (
	hashSlot    = make(chan struct{}, 1)
	hashWaiters atomic.Int32
)

// verifyPasswordHash runs one verification at a time: PBKDF2 takes about a
// second of CPU on a Raspberry Pi 4, and logins must not starve the audio
// pipeline. A login gives up when too many are already waiting or its request
// ends; the waiter cap also bounds how long a turn can take.
func verifyPasswordHash(ctx context.Context, password, hash string) bool {
	if hashWaiters.Add(1) > 16 {
		hashWaiters.Add(-1)
		slog.Warn("login rejected: password verification is busy")
		return false
	}
	defer hashWaiters.Add(-1)
	select {
	case hashSlot <- struct{}{}:
	case <-ctx.Done():
		slog.Warn("login rejected: password verification is busy")
		return false
	}
	defer func() { <-hashSlot }()
	return util.VerifyPasswordHash(password, hash)
}

// Login reports whether login succeeded and creates a session if valid.
func (sm *SessionManager) Login(
	w http.ResponseWriter, r *http.Request,
	username, password, configUser, configPass, configHash string,
) bool {
	userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(configUser)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(password), []byte(configPass)) == 1
	if configHash != "" {
		passMatch = configPass == "" && verifyPasswordHash(r.Context(), password, configHash)
	}
	if !userMatch || !passMatch {
		return false
	}

	token := sm.Create()
	if token == "" {
		return false
	}

	setSessionCookie(w, r, token, int(sessionDuration.Seconds()))
	return true
}

// Logout clears the session cookie and deletes the session.
func (sm *SessionManager) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		sm.Delete(cookie.Value)
	}
	setSessionCookie(w, r, "", -1)
}

// CreateCSRFToken generates a new CSRF token.
func (sm *SessionManager) CreateCSRFToken() string {
	token := generateToken()
	if token == "" {
		return ""
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()

	// Periodically clean up expired tokens.
	if rand.IntN(10) == 0 { //nolint:gosec // Non-security random for cleanup timing
		maps.DeleteFunc(sm.csrfTokens, func(_ string, v *csrfToken) bool {
			return now.After(v.expiresAt)
		})
	}
	if len(sm.csrfTokens) >= 4096 {
		maps.DeleteFunc(sm.csrfTokens, func(_ string, v *csrfToken) bool { return !now.Before(v.expiresAt) })
		if len(sm.csrfTokens) >= 4096 {
			// Anonymous GETs must not prevent every new user from obtaining a login form.
			var oldestToken string
			var oldestExpiry time.Time
			for key, value := range sm.csrfTokens {
				if oldestToken == "" || value.expiresAt.Before(oldestExpiry) {
					oldestToken, oldestExpiry = key, value.expiresAt
				}
			}
			delete(sm.csrfTokens, oldestToken)
		}
	}

	sm.csrfTokens[token] = &csrfToken{
		expiresAt: now.Add(csrfTokenDuration),
	}
	return token
}

// ValidateCSRFToken reports whether a CSRF token is valid and removes it.
func (sm *SessionManager) ValidateCSRFToken(token string) bool {
	if token == "" {
		return false
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	csrf, exists := sm.csrfTokens[token]
	if !exists {
		return false
	}

	delete(sm.csrfTokens, token)

	return time.Now().Before(csrf.expiresAt)
}
