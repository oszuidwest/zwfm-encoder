package types

import (
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// Webhook validation rule identifiers. Stored in validation.Issue.Code as
// stable rule identifiers; adapters in config/ and notify/ switch on these
// to map rules to context-specific messages.
const (
	// WebhookURLRequired means the webhook URL is empty.
	WebhookURLRequired = "url_required"
	// WebhookURLInvalid means the webhook URL is not a parseable request URI.
	WebhookURLInvalid = "url_invalid"
)

// ValidateWebhookURL reports webhook URL validation issues.
//
// In AllowEmpty mode, an empty URL is accepted; a non-empty URL must parse as
// a request URI. In RequireComplete mode, the URL must be non-empty; the
// format is not re-checked (preserving the historical behavior where invalid
// URLs surface as runtime errors from the HTTP layer rather than preflight
// validation).
func ValidateWebhookURL(rawURL string, mode validation.Mode) validation.Issues {
	if rawURL == "" {
		if mode == validation.RequireComplete {
			return validation.Issues{{Field: "url", Code: WebhookURLRequired}}
		}
		return nil
	}
	if mode == validation.AllowEmpty {
		if _, err := url.ParseRequestURI(rawURL); err != nil {
			return validation.Issues{{Field: "url", Code: WebhookURLInvalid}}
		}
	}
	return nil
}
