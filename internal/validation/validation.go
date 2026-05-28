// Package validation provides neutral primitives for domain-local validation.
//
// Each domain (notifications, etc.) defines its own typed Code constants and
// validation functions that return []Issue. Adapters at each call site map
// those issues to the error shape required by the caller (config-path message,
// API field name, runtime sentinel-wrapped error, etc.).
package validation

// Issue describes one validation failure.
//
// Codes are owned by the calling domain. Domains define typed string constants
// and place them into Code via explicit string conversion. The Field name is
// domain-local (for example "url"); adapters prefix it when formatting
// messages for a specific context.
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
