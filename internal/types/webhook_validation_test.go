package types

import (
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

func TestValidateWebhookURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		mode      validation.Mode
		wantCodes []WebhookValidationCode
	}{
		{
			name:      "AllowEmpty accepts empty URL",
			url:       "",
			mode:      validation.AllowEmpty,
			wantCodes: nil,
		},
		{
			name:      "AllowEmpty accepts valid URL",
			url:       "https://hooks.example.com/silence",
			mode:      validation.AllowEmpty,
			wantCodes: nil,
		},
		{
			name:      "AllowEmpty rejects unparseable URL",
			url:       "not a url",
			mode:      validation.AllowEmpty,
			wantCodes: []WebhookValidationCode{WebhookURLInvalid},
		},
		{
			name:      "RequireComplete rejects empty URL",
			url:       "",
			mode:      validation.RequireComplete,
			wantCodes: []WebhookValidationCode{WebhookURLRequired},
		},
		{
			name:      "RequireComplete accepts any non-empty URL without format check",
			url:       "not a url",
			mode:      validation.RequireComplete,
			wantCodes: nil,
		},
		{
			name:      "RequireComplete accepts valid URL",
			url:       "https://hooks.example.com/silence",
			mode:      validation.RequireComplete,
			wantCodes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := webhookCodes(ValidateWebhookURL(tt.url, tt.mode))
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ValidateWebhookURL(%q, %v) codes = %v, want %v", tt.url, tt.mode, got, tt.wantCodes)
			}
		})
	}
}

func webhookCodes(issues validation.Issues) []WebhookValidationCode {
	if len(issues) == 0 {
		return nil
	}
	codes := make([]WebhookValidationCode, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, WebhookValidationCode(issue.Code))
	}
	return codes
}
