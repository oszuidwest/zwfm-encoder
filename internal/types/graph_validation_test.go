package types

import (
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

const (
	graphValidGUIDA = "11111111-1111-1111-1111-111111111111"
	graphValidGUIDB = "22222222-2222-2222-2222-222222222222"
)

func TestGraphCredentialsIssues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       GraphConfig
		wantCodes []string
	}{
		{
			name:      "empty triggers all three required",
			cfg:       GraphConfig{},
			wantCodes: []string{GraphTenantIDRequired, GraphClientIDRequired, GraphClientSecretRequired},
		},
		{
			name:      "non-GUID tenant accepted (non-strict)",
			cfg:       GraphConfig{TenantID: "mycompany.onmicrosoft.com", ClientID: "client", ClientSecret: "secret"},
			wantCodes: nil,
		},
		{
			name:      "GUID tenant accepted",
			cfg:       GraphConfig{TenantID: graphValidGUIDA, ClientID: graphValidGUIDB, ClientSecret: "secret"},
			wantCodes: nil,
		},
		{
			name:      "only tenant set",
			cfg:       GraphConfig{TenantID: "tenant"},
			wantCodes: []string{GraphClientIDRequired, GraphClientSecretRequired},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := graphCodes(tt.cfg.CredentialsIssues())
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("CredentialsIssues() codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}

func TestGraphClientIssues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       GraphConfig
		wantCodes []string
	}{
		{
			name:      "empty triggers credentials and from",
			cfg:       GraphConfig{},
			wantCodes: []string{GraphTenantIDRequired, GraphClientIDRequired, GraphClientSecretRequired, GraphFromAddressRequired},
		},
		{
			name:      "credentials set but from missing",
			cfg:       GraphConfig{TenantID: graphValidGUIDA, ClientID: graphValidGUIDB, ClientSecret: "s"},
			wantCodes: []string{GraphFromAddressRequired},
		},
		{
			name:      "non-GUID tenant + from set: no issues (non-strict)",
			cfg:       GraphConfig{TenantID: "mycompany.onmicrosoft.com", ClientID: "client", ClientSecret: "s", FromAddress: "from@example.com"},
			wantCodes: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := graphCodes(tt.cfg.ClientIssues())
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ClientIssues() codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}

func TestGraphSendIssues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       GraphConfig
		wantCodes []string
	}{
		{
			name:      "empty triggers all five required (no format checks on empty)",
			cfg:       GraphConfig{},
			wantCodes: []string{GraphTenantIDRequired, GraphClientIDRequired, GraphClientSecretRequired, GraphFromAddressRequired, GraphRecipientsRequired},
		},
		{
			name:      "non-GUID tenant rejected (strict)",
			cfg:       GraphConfig{TenantID: "mycompany.onmicrosoft.com", ClientID: graphValidGUIDB, ClientSecret: "s", FromAddress: "from@example.com", Recipients: "to@example.com"},
			wantCodes: []string{GraphTenantIDFormat},
		},
		{
			name:      "non-GUID client rejected (strict)",
			cfg:       GraphConfig{TenantID: graphValidGUIDA, ClientID: "not-a-guid", ClientSecret: "s", FromAddress: "from@example.com", Recipients: "to@example.com"},
			wantCodes: []string{GraphClientIDFormat},
		},
		{
			name:      "all valid GUIDs and fields: no issues",
			cfg:       GraphConfig{TenantID: graphValidGUIDA, ClientID: graphValidGUIDB, ClientSecret: "s", FromAddress: "from@example.com", Recipients: "to@example.com"},
			wantCodes: nil,
		},
		{
			name:      "empty from and recipients with valid credentials",
			cfg:       GraphConfig{TenantID: graphValidGUIDA, ClientID: graphValidGUIDB, ClientSecret: "s"},
			wantCodes: []string{GraphFromAddressRequired, GraphRecipientsRequired},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := graphCodes(tt.cfg.SendIssues())
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("SendIssues() codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}

func graphCodes(issues validation.Issues) []string {
	if len(issues) == 0 {
		return nil
	}
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	return codes
}
