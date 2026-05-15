package util

import "strings"

var whatsAppPhoneReplacer = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")

// ParseWhatsAppRecipients splits a comma-separated phone number list and returns
// WhatsApp Cloud API-ready numbers without a leading plus sign.
func ParseWhatsAppRecipients(recipients string) []string {
	result := []string{}
	for recipient := range strings.SplitSeq(recipients, ",") {
		if normalized := NormalizeWhatsAppRecipient(recipient); normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

// NormalizeWhatsAppRecipient removes common phone-number separators.
func NormalizeWhatsAppRecipient(recipient string) string {
	trimmed := strings.TrimSpace(recipient)
	trimmed = strings.TrimPrefix(trimmed, "+")
	return whatsAppPhoneReplacer.Replace(trimmed)
}

// ValidWhatsAppRecipient reports whether recipient is a plausible WhatsApp
// Cloud API phone number in E.164 digit form, with or without a leading plus.
func ValidWhatsAppRecipient(recipient string) bool {
	normalized := NormalizeWhatsAppRecipient(recipient)
	if len(normalized) < 8 || len(normalized) > 15 {
		return false
	}
	return AllDigits(normalized)
}

// AllDigits reports whether s is non-empty and contains only ASCII digits.
func AllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ValidateWhatsAppRecipients validates a comma-separated recipient list.
func ValidateWhatsAppRecipients(field, recipients string) string {
	if recipients == "" {
		return ""
	}
	for recipient := range strings.SplitSeq(recipients, ",") {
		if trimmed := strings.TrimSpace(recipient); trimmed != "" && !ValidWhatsAppRecipient(trimmed) {
			return field + ": contains invalid phone number"
		}
	}
	return ""
}

// ValidWhatsAppTemplateName reports whether name matches Meta's lowercase
// alphanumeric plus underscore template-name format.
func ValidWhatsAppTemplateName(name string) bool {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return name != ""
}
