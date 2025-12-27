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

// session is an authenticated user session.
type session struct {
	expiresAt time.Time
}

// csrfToken is a CSRF token with expiration.
type csrfToken struct {
	expiresAt time.Time
}

// SessionManager handles user authentication sessions.
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

// generateToken creates a cryptographically secure random token.
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

	sm.mu.RLock()
	sess, exists := sm.sessions[token]
	sm.mu.RUnlock()

	if !exists {
		return false
	}

	if time.Now().After(sess.expiresAt) {
		sm.mu.Lock()
		delete(sm.sessions, token)
		sm.mu.Unlock()
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
func (sm *SessionManager) AuthMiddleware(username, password string) func(http.HandlerFunc) http.HandlerFunc {
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

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	return true
}

// Logout clears the session cookie and deletes the session.
func (sm *SessionManager) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		sm.Delete(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
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
