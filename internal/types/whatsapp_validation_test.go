package types

import (
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

func TestWhatsAppConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       *WhatsAppConfig
		mode      validation.Mode
		wantCodes []WhatsAppValidationCode
		wantValue string
	}{
		{
			name:      "nil allowed",
			cfg:       nil,
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{},
		},
		{
			name:      "empty allowed",
			cfg:       &WhatsAppConfig{},
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{},
		},
		{
			name:      "nil requires complete config",
			cfg:       nil,
			mode:      validation.RequireComplete,
			wantCodes: []WhatsAppValidationCode{WhatsAppConfigRequired},
		},
		{
			name: "partial config requires token",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				Recipients:    "+31612345678",
			},
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{WhatsAppAccessTokenRequired},
		},
		{
			name: "phone number id must contain digits only",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "abc",
				AccessToken:   "token",
				Recipients:    "+31612345678",
			},
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{WhatsAppPhoneNumberIDDigits},
		},
		{
			name: "invalid recipient is normalized",
			cfg: &WhatsAppConfig{
				Recipients: "bad-address",
			},
			mode: validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{
				WhatsAppRecipientInvalid,
				WhatsAppPhoneNumberIDRequired,
				WhatsAppAccessTokenRequired,
			},
			wantValue: "badaddress",
		},
		{
			name: "separator-only invalid recipient keeps original value",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   "token",
				Recipients:    "+31612345678,---",
			},
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{WhatsAppRecipientInvalid},
			wantValue: "---",
		},
		{
			name: "empty split recipients required",
			cfg: &WhatsAppConfig{
				PhoneNumberID: "12345",
				AccessToken:   "token",
				Recipients:    ",,, ",
			},
			mode:      validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{WhatsAppRecipientsRequired},
		},
		{
			name: "template language requires name and no whitespace",
			cfg: &WhatsAppConfig{
				PhoneNumberID:    "12345",
				AccessToken:      "token",
				Recipients:       "+31612345678",
				TemplateLanguage: "nl NL",
			},
			mode: validation.AllowEmpty,
			wantCodes: []WhatsAppValidationCode{
				WhatsAppTemplateLanguageRequiresName,
				WhatsAppTemplateLanguageWhitespace,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issues := tt.cfg.Validate(tt.mode)
			if got := whatsAppValidationCodes(issues); !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("Validate() codes = %v, want %v", got, tt.wantCodes)
			}
			if tt.wantValue != "" && issues[0].Value != tt.wantValue {
				t.Fatalf("Validate() first value = %q, want %q", issues[0].Value, tt.wantValue)
			}
		})
	}
}

func whatsAppValidationCodes(issues validation.Issues) []WhatsAppValidationCode {
	codes := make([]WhatsAppValidationCode, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, WhatsAppValidationCode(issue.Code))
	}
	return codes
}
