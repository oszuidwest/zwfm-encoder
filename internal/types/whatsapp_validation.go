package types

import (
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

const whatsAppWhitespaceChars = " \t\n\r\f\v"

// WhatsAppValidationCode identifies a WhatsApp validation rule failure.
// Stored in validation.Issue.Code via string conversion; adapters cast
// back to this type when switching on rule identity.
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

// Validate reports WhatsApp configuration validation issues.
func (c *WhatsAppConfig) Validate(mode validation.Mode) validation.Issues {
	if c == nil {
		if mode == validation.RequireComplete {
			return validation.Issues{{Code: string(WhatsAppConfigRequired)}}
		}
		return validation.Issues{}
	}

	issues := validation.Issues{}
	if invalidRecipient, ok := util.FirstInvalidWhatsAppRecipient(c.Recipients); ok {
		issues = append(issues, validation.Issue{
			Field: "recipients",
			Code:  string(WhatsAppRecipientInvalid),
			Value: invalidRecipient,
		})
	}

	phoneNumberID := strings.TrimSpace(c.PhoneNumberID)
	accessToken := strings.TrimSpace(c.AccessToken)
	recipients := strings.TrimSpace(c.Recipients)
	templateName := strings.TrimSpace(c.TemplateName)
	templateLanguage := strings.TrimSpace(c.TemplateLanguage)

	if mode == validation.AllowEmpty && !hasAnyWhatsAppConfig(
		phoneNumberID,
		accessToken,
		recipients,
		templateName,
		templateLanguage,
	) {
		return issues
	}

	if phoneNumberID == "" {
		issues = append(issues, validation.Issue{Field: "phone_number_id", Code: string(WhatsAppPhoneNumberIDRequired)})
	} else if !util.AllDigits(phoneNumberID) {
		issues = append(issues, validation.Issue{Field: "phone_number_id", Code: string(WhatsAppPhoneNumberIDDigits)})
	}
	if accessToken == "" {
		issues = append(issues, validation.Issue{Field: "access_token", Code: string(WhatsAppAccessTokenRequired)})
	}
	if len(util.ParseWhatsAppRecipients(recipients)) == 0 {
		issues = append(issues, validation.Issue{Field: "recipients", Code: string(WhatsAppRecipientsRequired)})
	}
	if templateName != "" && !util.ValidWhatsAppTemplateName(templateName) {
		issues = append(issues, validation.Issue{Field: "template_name", Code: string(WhatsAppTemplateNameFormat)})
	}
	if templateLanguage != "" {
		if templateName == "" {
			issues = append(issues, validation.Issue{
				Field: "template_language",
				Code:  string(WhatsAppTemplateLanguageRequiresName),
			})
		}
		if strings.ContainsAny(templateLanguage, whatsAppWhitespaceChars) {
			issues = append(issues, validation.Issue{
				Field: "template_language",
				Code:  string(WhatsAppTemplateLanguageWhitespace),
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
