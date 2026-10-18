// Package validation provides neutral primitives for domain-local validation.
//
// Each domain (notifications, etc.) defines its own Code constants and
// validation functions that return []Issue. Adapters at each call site map
// those issues to the error shape required by the caller (config-path message,
// API field name, runtime sentinel-wrapped error, etc.).
package validation

import (
	"errors"
	"fmt"
	"net/netip"
)

// Issue describes one validation failure.
//
// Codes are owned by the calling domain. Domains define stable string
// constants for each rule (for example "url_required") and place them
// directly into Code. The Field name is domain-local (for example "url");
// adapters prefix it when formatting messages for a specific context.
type Issue struct {
	Field string
	Code  string
	// Value carries the offending input when useful to the message.
	// Most issues leave it empty.
	Value string
}

// Mode controls whether a mode-aware domain validator tolerates an empty
// configuration. Per-domain semantics are documented at each Validate function.
type Mode int

const (
	// AllowEmpty accepts a fully empty configuration. Each domain defines what
	// "empty" means (all fields blank, no activation field set, etc.) and what
	// follows when the configuration is non-empty.
	AllowEmpty Mode = iota
	// RequireComplete requires all fields needed to perform the action.
	RequireComplete
)

// Issues is a slice of Issue. Defined as a named type so future helpers can
// hang off it without disturbing call sites.
type Issues []Issue

// ValidPort reports whether p is a usable TCP/UDP port number (1..65535).
func ValidPort(p int) bool {
	return p >= 1 && p <= 65535
}

// TrustedProxyPrefix parses a trusted proxy CIDR. It rejects /0, which would
// trust every peer, and IPv4-mapped prefixes, because peers are matched in
// their unmapped form.
func TrustedProxyPrefix(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Bits() == 0 {
		return netip.Prefix{}, errors.New("must contain explicit proxy CIDRs, never /0")
	}
	if prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("use plain IPv4 CIDRs instead of IPv4-mapped IPv6 %q", value)
	}
	return prefix, nil
}
