package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestLoginClientAddressTrustBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, peer, header, want string
		trusted                  []string
	}{
		{name: "direct", peer: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "no implicit loopback trust", peer: "127.0.0.1:1234", header: "198.51.100.1", want: "127.0.0.1"},
		{name: "untrusted forged header", peer: "192.0.2.1:1234", header: "198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "192.0.2.1"},
		{name: "trusted proxy", peer: "127.0.0.1:1234", header: "198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "198.51.100.1"},
		{name: "forged leftmost", peer: "127.0.0.1:1234", header: "203.0.113.99, 198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "198.51.100.1"},
		{name: "trusted chain", peer: "127.0.0.1:1234", header: "203.0.113.99, 198.51.100.1, 10.0.0.2", trusted: []string{"127.0.0.1/32", "10.0.0.2/32"}, want: "198.51.100.1"},
		{name: "untrusted intermediate", peer: "127.0.0.1:1234", header: "198.51.100.1, 10.0.0.2", trusted: []string{"127.0.0.1/32"}, want: "10.0.0.2"},
		{name: "IPv6", peer: "[::1]:1234", header: "2001:db8::8", trusted: []string{"::1/128"}, want: "2001:db8::8"},
		{name: "mapped peer", peer: "[::ffff:127.0.0.1]:1234", header: "198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "198.51.100.1"},
		{name: "missing header", peer: "127.0.0.1:1234", trusted: []string{"127.0.0.1/32"}, want: "127.0.0.1"},
		{name: "malformed header", peer: "127.0.0.1:1234", header: "unknown", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "header with port", peer: "127.0.0.1:1234", header: "198.51.100.1:1234", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "empty hop", peer: "127.0.0.1:1234", header: "198.51.100.1, ", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "unspecified", peer: "127.0.0.1:1234", header: "0.0.0.0", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "multicast", peer: "127.0.0.1:1234", header: "224.0.0.251", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "oversized header", peer: "127.0.0.1:1234", header: strings.Repeat(" ", 4096) + "198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
		{name: "too many hops", peer: "127.0.0.1:1234", header: strings.Repeat("10.0.0.2,", 32) + "198.51.100.1", trusted: []string{"127.0.0.1/32", "10.0.0.2/32"}, want: "invalid IP"},
		{name: "invalid trust fails closed", peer: "127.0.0.1:1234", header: "198.51.100.1", trusted: []string{"invalid", "0.0.0.0/0"}, want: "127.0.0.1"},
		{name: "mapped peer needs IPv4 trust", peer: "[::ffff:127.0.0.1]:1234", header: "198.51.100.1", trusted: []string{"::/1"}, want: "::ffff:127.0.0.1"},
		{name: "mapped trust fails closed", peer: "[::ffff:127.0.0.1]:1234", header: "203.0.113.99, ::ffff:198.51.100.1", trusted: []string{"::ffff:0:0/96"}, want: "::ffff:127.0.0.1"},
		{name: "invalid peer", peer: "invalid", header: "198.51.100.1", trusted: []string{"127.0.0.1/32"}, want: "invalid IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.header)
			if got := loginClientAddress(r, tc.trusted).String(); got != tc.want {
				t.Fatalf("client address = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUsernameLimitExpiresDuringRejectedTraffic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sm := NewSessionManager()
		r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
		for i := range 10 {
			r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
			if sm.AllowLogin(r, "admin", nil) != nil {
				t.Fatal("early username throttle")
			}
		}
		r.RemoteAddr = "198.51.100.1:1234"
		for range 5 {
			time.Sleep(10 * time.Second)
			if sm.AllowLogin(r, "admin", nil) == nil {
				t.Fatal("username budget bypassed")
			}
		}
		time.Sleep(10 * time.Second)
		if sm.AllowLogin(r, "admin", nil) != nil {
			t.Fatal("rejected traffic prolonged the username throttle")
		}
	})
}

func TestLoginRepeatedForwardingHeaders(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Add("X-Forwarded-For", "203.0.113.99")
	r.Header.Add("X-Forwarded-For", "198.51.100.1")
	if got := loginClientAddress(r, []string{"127.0.0.1/32"}).String(); got != "198.51.100.1" {
		t.Fatalf("client address = %s", got)
	}
}

func TestMalformedForwardingCannotExhaustProxyBudget(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager()
	r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
	r.RemoteAddr = "127.0.0.1:1234"
	trusted := []string{"127.0.0.1/32"}
	for range 20 {
		r.Header.Set("X-Forwarded-For", "unparseable")
		if sm.AllowLogin(r, "admin", trusted) == nil {
			t.Fatal("malformed forwarding was accepted")
		}
	}
	if len(sm.loginAttempts) != 0 {
		t.Fatal("malformed forwarding consumed shared proxy or username budget")
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if sm.AllowLogin(r, "admin", trusted) != nil {
		t.Fatal("malformed clients blocked a legitimate proxy client")
	}
}

func TestFullLoginCacheKeepsCurrentLimits(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager()
	r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
	r.RemoteAddr = "192.0.2.1:1234"
	for i := range maxLoginBuckets - 1 {
		sm.loginAttempts[fmt.Sprint(i)] = loginAttempt{count: 1, expiresAt: time.Now().Add(time.Minute)}
	}
	// The oldest entry is the current IP: eviction must not reset its four attempts.
	sm.loginAttempts["ip:192.0.2.1"] = loginAttempt{count: 4, expiresAt: time.Now().Add(30 * time.Second)}
	if sm.AllowLogin(r, "admin", nil) != nil || len(sm.loginAttempts) != maxLoginBuckets {
		t.Fatal("new username was globally denied by a full cache")
	}
	if sm.AllowLogin(r, "different", nil) == nil {
		t.Fatal("cache pressure reset the current IP limit")
	}
	if sm.loginAttempts["ip:192.0.2.1"].count != 5 {
		t.Fatal("eviction lost the active counter")
	}
}

func TestLoginEvictionKeepsExhaustedBuckets(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager()
	now := time.Now()
	// The exhausted account bucket is the oldest, so plain oldest-first eviction would reset it.
	sm.loginAttempts["user:admin"] = loginAttempt{count: 10, expiresAt: now.Add(30 * time.Second)}
	for i := range maxLoginBuckets - 1 {
		sm.loginAttempts[fmt.Sprint("ip:", i)] = loginAttempt{count: 1, expiresAt: now.Add(time.Minute)}
	}
	r := httptest.NewRequest(http.MethodPost, "/login", http.NoBody)
	r.RemoteAddr = "192.0.2.9:1234"
	if sm.AllowLogin(r, "guess", nil) != nil {
		t.Fatal("new client refused although unexhausted buckets can be evicted")
	}
	if sm.loginAttempts["user:admin"].count != 10 {
		t.Fatal("eviction reset an exhausted account limit")
	}
}
