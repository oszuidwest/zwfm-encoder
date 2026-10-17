package server

import (
	"maps"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const (
	loginWindow     = time.Minute
	maxLoginBuckets = 2048
)

type loginAttempt struct {
	count     int
	expiresAt time.Time
}

// AllowLogin reserves an attempt against both peer IP and submitted username.
// Forwarded IP headers are deliberately ignored; proxies share the peer limit.
func (sm *SessionManager) AllowLogin(r *http.Request, username string) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	keys := [2]string{"ip:" + host, "user:" + username}
	limits := [2]int{5, 10}
	now := time.Now()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	maps.DeleteFunc(sm.loginAttempts, func(_ string, attempt loginAttempt) bool {
		return !now.Before(attempt.expiresAt)
	})
	newBuckets := 0
	for i, key := range keys {
		attempt, exists := sm.loginAttempts[key]
		if attempt.count >= limits[i] {
			return false
		}
		if !exists {
			newBuckets++
		}
	}
	if len(sm.loginAttempts)+newBuckets > maxLoginBuckets {
		return false
	}
	for _, key := range keys {
		attempt := sm.loginAttempts[key]
		if attempt.count == 0 {
			attempt.expiresAt = now.Add(loginWindow)
		}
		attempt.count++
		sm.loginAttempts[key] = attempt
	}
	return true
}
