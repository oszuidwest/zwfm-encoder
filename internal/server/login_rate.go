package server

import (
	"maps"
	"net"
	"net/http"
	"net/netip"
	"strings"
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

// AllowLogin reserves an attempt against both client network and submitted username.
func (sm *SessionManager) AllowLogin(r *http.Request, username string, trustedProxies []string) bool {
	ip := loginClientAddress(r, trustedProxies).Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	host := ip.String()
	if ip.Is6() {
		host = netip.PrefixFrom(ip, 64).Masked().String()
	}
	keys := [2]string{"ip:" + host, "user:" + username}
	// The account window is fixed: rejected attempts must never prolong it.
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
	for len(sm.loginAttempts)+newBuckets > maxLoginBuckets {
		sm.evictLoginBucket(keys)
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

// evictLoginBucket keeps a full cache from blocking unrelated clients; sm.mu is held.
func (sm *SessionManager) evictLoginBucket(keep [2]string) {
	var oldestKey string
	var oldestExpiry time.Time
	for key, value := range sm.loginAttempts {
		if key == keep[0] || key == keep[1] {
			continue
		}
		if oldestKey == "" || value.expiresAt.Before(oldestExpiry) {
			oldestKey, oldestExpiry = key, value.expiresAt
		}
	}
	delete(sm.loginAttempts, oldestKey)
}

func loginClientAddress(r *http.Request, trustedProxies []string) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	if !isTrustedProxy(peer, trustedProxies) {
		return peer
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if forwarded == "" {
		return peer
	}
	if len(forwarded) > 4096 {
		return netip.Addr{}
	}
	hops := strings.Split(forwarded, ",")
	if len(hops) > 32 {
		return netip.Addr{}
	}
	client := peer
	// Stop at the first untrusted hop; a client can forge everything to its left.
	for i := len(hops) - 1; i >= 0 && isTrustedProxy(client, trustedProxies); i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || next.IsUnspecified() || next.IsMulticast() {
			return netip.Addr{}
		}
		client = next
	}
	return client
}

func isTrustedProxy(ip netip.Addr, trustedProxies []string) bool {
	for _, value := range trustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && prefix.Bits() > 0 && (prefix.Contains(ip) || prefix.Contains(ip.Unmap())) {
			return true
		}
	}
	return false
}
