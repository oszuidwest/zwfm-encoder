package types

import (
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// WebhookValidationCode identifies a webhook validation rule failure.
// Stored in validation.Issue.Code via string conversion; adapters cast
// back when switching on rule identity.
type WebhookValidationCode string

const (
	// WebhookURLRequired means the webhook URL is empty.
	WebhookURLRequired WebhookValidationCode = "url_required"
	// WebhookURLInvalid means the webhook URL is not a parseable request URI.
	WebhookURLInvalid WebhookValidationCode = "url_invalid"
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
			return validation.Issues{{Field: "url", Code: string(WebhookURLRequired)}}
		}
		return nil
	}
	if mode == validation.AllowEmpty {
		if _, err := url.ParseRequestURI(rawURL); err != nil {
			return validation.Issues{{Field: "url", Code: string(WebhookURLInvalid)}}
		}
	}
	return nil
}
