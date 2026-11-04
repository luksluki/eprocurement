package tenant

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestWithTenantRejectsEmptyTenant(t *testing.T) {
	err := WithTenant(t.Context(), nil, "", func(tx pgx.Tx) error { return nil })
	if err == nil || err.Error() != "tenant: WithTenant called with empty tenant id" {
		t.Fatalf("err = %v, want empty-tenant rejection", err)
	}
}
