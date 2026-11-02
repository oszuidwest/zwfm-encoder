package notify

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestNewMailClientPreservesClientValidation(t *testing.T) {
	t.Parallel()
	_, err := newMailClient(&types.GraphConfig{
		TenantID:     "tenant.onmicrosoft.com",
		ClientID:     "client-id",
		ClientSecret: "secret",
	})
	if err == nil || err.Error() != "from address (shared mailbox) is required" {
		t.Fatalf("missing from_address error = %v, want legacy from_address error", err)
	}
}
