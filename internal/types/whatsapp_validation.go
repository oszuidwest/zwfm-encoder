package types

import (
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const whatsAppWhitespaceChars = " \t\n\r\f\v"

// WhatsAppValidationMode controls whether an empty WhatsApp configuration is allowed.
type WhatsAppValidationMode int

const (
	// WhatsAppAllowEmpty allows a completely empty WhatsApp configuration.
	WhatsAppAllowEmpty WhatsAppValidationMode = iota
	// WhatsAppRequireComplete requires all fields needed to send WhatsApp messages.
	WhatsAppRequireComplete
)

// WhatsAppValidationCode identifies a WhatsApp validation rule failure.
type WhatsAppValidationCode string

const (
	// WhatsAppConfigRequired means the configuration object itself is missing.
	WhatsAppConfigRequired WhatsAppValidationCode = "config_required"
	// WhatsAppPhoneNumberIDRequired means the phone number ID is empty.
	WhatsAppPhoneNumberIDRequired WhatsAppValidationCode = "phone_number_id_required"
	// WhatsAppPhoneNumberIDDigits means the phone number ID contains non-digits.
	WhatsAppPhoneNumberIDDigits WhatsAppValidationCode = "phone_number_id_digits"
	// WhatsAppAccessTokenRequired means the access token is empty.
	WhatsAppAccessTokenRequired WhatsAppValidationCode = "access_token_required"
	// WhatsAppRecipientsRequired means no usable recipients were configured.
	WhatsAppRecipientsRequired WhatsAppValidationCode = "recipients_required"
	// WhatsAppRecipientInvalid means one recipient is not a valid WhatsApp phone number.
	WhatsAppRecipientInvalid WhatsAppValidationCode = "recipient_invalid"
	// WhatsAppTemplateNameFormat means the template name has an invalid format.
	WhatsAppTemplateNameFormat WhatsAppValidationCode = "template_name_format"
	// WhatsAppTemplateLanguageRequiresName means a language was set without a template name.
	WhatsAppTemplateLanguageRequiresName WhatsAppValidationCode = "template_language_requires_name"
	// WhatsAppTemplateLanguageWhitespace means the template language contains whitespace.
	WhatsAppTemplateLanguageWhitespace WhatsAppValidationCode = "template_language_whitespace"
)

// WhatsAppValidationIssue describes one WhatsApp configuration validation failure.
type WhatsAppValidationIssue struct {
	Field string
	Code  WhatsAppValidationCode
	// Value contains the invalid input value for codes that need it.
	Value string
}

// Validate reports WhatsApp configuration validation issues.
func (c *WhatsAppConfig) Validate(mode WhatsAppValidationMode) []WhatsAppValidationIssue {
	if c == nil {
		if mode == WhatsAppRequireComplete {
			return []WhatsAppValidationIssue{{Code: WhatsAppConfigRequired}}
		}
		return []WhatsAppValidationIssue{}
	}

	issues := []WhatsAppValidationIssue{}
	if invalidRecipient, ok := firstInvalidWhatsAppRecipient(c.Recipients); ok {
		issues = append(issues, WhatsAppValidationIssue{
			Field: "recipients",
			Code:  WhatsAppRecipientInvalid,
			Value: invalidRecipient,
		})
	}

	phoneNumberID := strings.TrimSpace(c.PhoneNumberID)
	accessToken := strings.TrimSpace(c.AccessToken)
	recipients := strings.TrimSpace(c.Recipients)
	templateName := strings.TrimSpace(c.TemplateName)
	templateLanguage := strings.TrimSpace(c.TemplateLanguage)

	if mode == WhatsAppAllowEmpty && !hasAnyWhatsAppConfig(
		phoneNumberID,
		accessToken,
		recipients,
		templateName,
		templateLanguage,
	) {
		return issues
	}

	if phoneNumberID == "" {
		issues = append(issues, WhatsAppValidationIssue{Field: "phone_number_id", Code: WhatsAppPhoneNumberIDRequired})
	} else if !util.AllDigits(phoneNumberID) {
		issues = append(issues, WhatsAppValidationIssue{Field: "phone_number_id", Code: WhatsAppPhoneNumberIDDigits})
	}
	if accessToken == "" {
		issues = append(issues, WhatsAppValidationIssue{Field: "access_token", Code: WhatsAppAccessTokenRequired})
	}
	if len(util.ParseWhatsAppRecipients(recipients)) == 0 {
		issues = append(issues, WhatsAppValidationIssue{Field: "recipients", Code: WhatsAppRecipientsRequired})
	}
	if templateName != "" && !util.ValidWhatsAppTemplateName(templateName) {
		issues = append(issues, WhatsAppValidationIssue{Field: "template_name", Code: WhatsAppTemplateNameFormat})
	}
	if templateLanguage != "" {
		if templateName == "" {
			issues = append(issues, WhatsAppValidationIssue{
				Field: "template_language",
				Code:  WhatsAppTemplateLanguageRequiresName,
			})
		}
		if strings.ContainsAny(templateLanguage, whatsAppWhitespaceChars) {
			issues = append(issues, WhatsAppValidationIssue{
				Field: "template_language",
				Code:  WhatsAppTemplateLanguageWhitespace,
			})
		}
	}

	return issues
}

func hasAnyWhatsAppConfig(fields ...string) bool {
	for _, field := range fields {
		if field != "" {
			return true
		}
	}
	return false
}

func firstInvalidWhatsAppRecipient(recipients string) (string, bool) {
	for recipient := range strings.SplitSeq(recipients, ",") {
		trimmed := strings.TrimSpace(recipient)
		if trimmed != "" && !util.ValidWhatsAppRecipient(trimmed) {
			normalized := util.NormalizeWhatsAppRecipient(trimmed)
			if normalized != "" {
				return normalized, true
			}
			return trimmed, true
		}
	}
	return "", false
}
