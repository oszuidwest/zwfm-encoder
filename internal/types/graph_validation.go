package types

import (
	"regexp"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// Microsoft Graph validation rule identifiers. Stored in validation.Issue.Code
// as stable rule identifiers; adapters in notify/ switch on these to map
// rules to context-specific messages.
const (
	// GraphTenantIDRequired means tenant_id is empty.
	GraphTenantIDRequired = "tenant_id_required"
	// GraphTenantIDFormat means tenant_id is not a valid GUID.
	GraphTenantIDFormat = "tenant_id_format"
	// GraphClientIDRequired means client_id is empty.
	GraphClientIDRequired = "client_id_required"
	// GraphClientIDFormat means client_id is not a valid GUID.
	GraphClientIDFormat = "client_id_format"
	// GraphClientSecretRequired means client_secret is empty.
	GraphClientSecretRequired = "client_secret_required"
	// GraphFromAddressRequired means from_address is empty.
	GraphFromAddressRequired = "from_address_required"
	// GraphRecipientsRequired means recipients is empty.
	GraphRecipientsRequired = "recipients_required"
)

// graphGUIDPattern matches the standard GUID format used by Azure AD
// tenant and client identifiers. Single source: notify package consumes
// strict validation via SendIssues rather than re-implementing the regex.
var graphGUIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// CredentialsIssues reports validation issues for tenant_id, client_id, and
// client_secret. GUID format is not enforced (preserves the historical
// behavior of validateCredentials(cfg, false), used by NewGraphClient and
// TokenSourceContext, where Azure tenant domain names are accepted).
func (c *GraphConfig) CredentialsIssues() validation.Issues {
	var issues validation.Issues
	if c.TenantID == "" {
		issues = append(issues, validation.Issue{Field: "tenant_id", Code: GraphTenantIDRequired})
	}
	if c.ClientID == "" {
		issues = append(issues, validation.Issue{Field: "client_id", Code: GraphClientIDRequired})
	}
	if c.ClientSecret == "" {
		issues = append(issues, validation.Issue{Field: "client_secret", Code: GraphClientSecretRequired})
	}
	return issues
}

// ClientIssues reports validation issues for the credentials plus from_address.
// Matches the historical preflight in NewGraphClient: credentials required
// (non-strict GUID) and from_address required.
func (c *GraphConfig) ClientIssues() validation.Issues {
	issues := c.CredentialsIssues()
	if c.FromAddress == "" {
		issues = append(issues, validation.Issue{Field: "from_address", Code: GraphFromAddressRequired})
	}
	return issues
}

// SendIssues reports validation issues for everything required to send mail:
// credentials with strict GUID format, from_address, and recipients. Matches
// the historical preflight in ValidateConfig used by test-send paths.
func (c *GraphConfig) SendIssues() validation.Issues {
	var issues validation.Issues
	if c.TenantID == "" {
		issues = append(issues, validation.Issue{Field: "tenant_id", Code: GraphTenantIDRequired})
	} else if !graphGUIDPattern.MatchString(c.TenantID) {
		issues = append(issues, validation.Issue{Field: "tenant_id", Code: GraphTenantIDFormat})
	}
	if c.ClientID == "" {
		issues = append(issues, validation.Issue{Field: "client_id", Code: GraphClientIDRequired})
	} else if !graphGUIDPattern.MatchString(c.ClientID) {
		issues = append(issues, validation.Issue{Field: "client_id", Code: GraphClientIDFormat})
	}
	if c.ClientSecret == "" {
		issues = append(issues, validation.Issue{Field: "client_secret", Code: GraphClientSecretRequired})
	}
	if c.FromAddress == "" {
		issues = append(issues, validation.Issue{Field: "from_address", Code: GraphFromAddressRequired})
	}
	if c.Recipients == "" {
		issues = append(issues, validation.Issue{Field: "recipients", Code: GraphRecipientsRequired})
	}
	return issues
}
