package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/oszuidwest/zw-graphmail"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// formatGraphRuntimeError maps the first issue to the runtime error wording.
func formatGraphRuntimeError(issues validation.Issues) error {
	if len(issues) == 0 {
		return nil
	}
	switch issues[0].Code {
	case types.GraphTenantIDRequired:
		return fmt.Errorf("tenant ID is required")
	case types.GraphTenantIDFormat:
		return fmt.Errorf("tenant ID must be a valid GUID (e.g., 12345678-1234-1234-1234-123456789abc)")
	case types.GraphClientIDRequired:
		return fmt.Errorf("client ID is required")
	case types.GraphClientIDFormat:
		return fmt.Errorf("client ID must be a valid GUID (e.g., 12345678-1234-1234-1234-123456789abc)")
	case types.GraphClientSecretRequired:
		return fmt.Errorf("client secret is required")
	case types.GraphFromAddressRequired:
		return fmt.Errorf("from address (shared mailbox) is required")
	case types.GraphRecipientsRequired:
		return fmt.Errorf("recipients are required")
	default:
		return fmt.Errorf("invalid Graph configuration")
	}
}

// mailer sends one mail. *graphmail.Client implements it.
type mailer interface {
	Send(ctx context.Context, m *graphmail.Message) error
}

// newMailClient validates cfg and creates a Graph mail client.
func newMailClient(cfg *types.GraphConfig) (*graphmail.Client, error) {
	if err := formatGraphRuntimeError(cfg.ClientIssues()); err != nil {
		return nil, err
	}
	return graphmail.New(graphmail.Config{
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		From:         cfg.FromAddress,
	})
}

// ValidateConfig validates that cfg has all required fields.
func ValidateConfig(cfg *types.GraphConfig) error {
	return formatGraphRuntimeError(cfg.SendIssues())
}

// ParseRecipients splits a comma-separated recipients string into a slice.
func ParseRecipients(recipients string) []string {
	var result []string
	for r := range strings.SplitSeq(recipients, ",") {
		if r = strings.TrimSpace(r); r != "" {
			result = append(result, r)
		}
	}
	return result
}
