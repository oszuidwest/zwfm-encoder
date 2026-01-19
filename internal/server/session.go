// Package server provides the HTTP server and WebSocket handler for the web interface.
package server

import (
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"maps"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "encoder_session"
	sessionDuration   = 24 * time.Hour
	csrfTokenDuration = 10 * time.Minute
)

// A session represents an authenticated user session.
type session struct {
	expiresAt time.Time
}

// A csrfToken represents a CSRF token with expiration.
type csrfToken struct {
	expiresAt time.Time
}

// SessionManager manages user authentication sessions and CSRF tokens.
// It is safe for concurrent use.
type SessionManager struct {
	sessions   map[string]*session
	csrfTokens map[string]*csrfToken
	mu         sync.RWMutex
}

// NewSessionManager creates a new session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:   make(map[string]*session),
		csrfTokens: make(map[string]*csrfToken),
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
	if token == "" {
		return ""
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.sessions[token] = &session{
		expiresAt: time.Now().Add(sessionDuration),
	}
	return token
}

// Validate reports whether a session token is valid.
func (sm *SessionManager) Validate(token string) bool {
	if token == "" {
		return false
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	sess, exists := sm.sessions[token]
	if !exists {
		return false
	}

	if time.Now().After(sess.expiresAt) {
		delete(sm.sessions, token)
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
			if cookie, err := r.Cookie(sessionCookieName); err == nil {
				if sm.Validate(cookie.Value) {
					next(w, r)
					return
				}
			}

			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}
}

// setSessionCookie sets or clears the session cookie.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// Login reports whether login succeeded and creates a session if valid.
func (sm *SessionManager) Login(w http.ResponseWriter, r *http.Request, username, password, configUser, configPass string) bool {
	userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(configUser)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(password), []byte(configPass)) == 1
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
	if rand.IntN(10) == 0 {
		maps.DeleteFunc(sm.csrfTokens, func(_ string, v *csrfToken) bool {
			return now.After(v.expiresAt)
		})
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
