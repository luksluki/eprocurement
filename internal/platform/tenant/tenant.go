// Package tenant scopes database access by tenant for PostgreSQL row-level
// security: every tenant-scoped query runs inside a transaction that sets the
// app.tenant_id session variable, which the RLS policies (identity migration
// 0002, and later the procurement ones) compare against.
//
// Superusers bypass RLS even with FORCE, so every transaction also switches
// to the non-superuser AppRole (created by migration 0003). What runs through
// this package is bound by the policies; what does not (raw pool access)
// must be treated as privileged.
package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppRole is the non-superuser runtime role for tenant-scoped access.
const AppRole = "eproc_app"

// WithTenant runs fn inside a transaction whose RLS context pins tenantID:
// only that tenant's rows are visible, and inserts of other tenants' rows
// fail. It is the only way application code should reach tenant data.
func WithTenant(ctx context.Context, db *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return fmt.Errorf("tenant: WithTenant called with empty tenant id")
	}
	return withSettings(ctx, db, map[string]string{"app.tenant_id": tenantID}, fn)
}

// WithBypass runs fn with the transaction-scoped app.rls_bypass flag, for the
// auth flows that legitimately span tenants (login email lookup, refresh,
// logout). Never wrap tenant-scoped business queries in this.
func WithBypass(ctx context.Context, db *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return withSettings(ctx, db, map[string]string{"app.rls_bypass": "on"}, fn)
}

func withSettings(ctx context.Context, db *pgxpool.Pool, settings map[string]string, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Drop superuser (and its RLS exemption) for this transaction.
	if _, err := tx.Exec(ctx, "SELECT set_config('role', $1, true)", AppRole); err != nil {
		return fmt.Errorf("tenant: set role %s: %w", AppRole, err)
	}
	for key, value := range settings {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", key, value); err != nil {
			return fmt.Errorf("tenant: set %s: %w", key, err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
