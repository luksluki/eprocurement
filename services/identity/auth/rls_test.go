package auth

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/identity/migrations"
)

// rlsDB returns a pool with the identity schema (incl. RLS migration) applied.
func rlsDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	migCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open migration pool: %v", err)
	}
	if _, err := dbmigrate.Apply(t.Context(), migPool, "identity", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seedTenantUser inserts a user under WithBypass (test fixtures span tenants).
func seedTenantUser(t *testing.T, db *pgxpool.Pool, tenantID, email string) string {
	t.Helper()
	hash, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id string
	err = tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO identity.users (tenant_id, email, display_name, password_hash)
			VALUES ($1, $2, 'RLS Test', $3)
			ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET password_hash = EXCLUDED.password_hash
			RETURNING id`, tenantID, email, hash).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM identity.users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// visibleUserCount counts identity.users as seen from inside the given
// tenant-scoped transaction.
func visibleUserCount(t *testing.T, db *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var count int
	err := tenant.WithTenant(t.Context(), db, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM identity.users`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count under tenant: %v", err)
	}
	return count
}

const (
	rlsTenantA = "aaaaaaaa-0000-0000-0000-000000000001"
	rlsTenantB = "aaaaaaaa-0000-0000-0000-000000000002"
)

// AC: cross-tenant reads fail — a tenant-scoped query sees only its tenant.
func TestRLSCrossTenantReadsScoped(t *testing.T) {
	db := rlsDB(t)
	emailA := "rls-a@login.test"
	seedTenantUser(t, db, rlsTenantA, emailA)
	seedTenantUser(t, db, rlsTenantB, "rls-b@login.test")

	a := visibleUserCount(t, db, rlsTenantA)
	b := visibleUserCount(t, db, rlsTenantB)
	if a != 1 || b != 1 {
		t.Fatalf("tenant A sees %d users, tenant B sees %d; want 1 each (cross-tenant rows invisible)", a, b)
	}
}

// AC: RLS blocks bypass attempts — a query running as the application role
// (what the tenant helpers enforce) with NO tenant context sees nothing, even
// though the table owner (superuser, migrations) can see everything.
func TestRLSBlocksWithoutTenantContext(t *testing.T) {
	db := rlsDB(t)
	seedTenantUser(t, db, rlsTenantA, "rls-none@login.test")

	// Application posture: non-superuser role, no app.tenant_id set.
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), "SELECT set_config('role', $1, true)", tenant.AppRole); err != nil {
		t.Fatalf("set app role: %v", err)
	}
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM identity.users`).Scan(&count); err != nil {
		t.Fatalf("plain count: %v", err)
	}
	if count != 0 {
		t.Fatalf("plain query saw %d users without tenant context; want 0 (RLS must block)", count)
	}
}

// AC: cross-tenant writes fail — inserting another tenant's row inside a
// tenant-scoped transaction violates the WITH CHECK policy.
func TestRLSCrossTenantWriteRejected(t *testing.T) {
	db := rlsDB(t)
	// If a regression ever lets the insert through, remove its row so this
	// test does not poison the read/switching assertions.
	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM identity.users WHERE email = 'intruder@login.test'`)
			return err
		})
	})

	err := tenant.WithTenant(t.Context(), db, rlsTenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `
			INSERT INTO identity.users (tenant_id, email, display_name, password_hash)
			VALUES ($1, 'intruder@login.test', 'x', 'hash')`, rlsTenantB)
		return err
	})
	if err == nil {
		t.Fatal("cross-tenant insert was accepted; want RLS violation")
	}
	if !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("err = %v, want row-level security violation", err)
	}
}

// AC: tenant switching works for a demo admin — the same caller, scoped to
// tenant A then tenant B, sees each tenant's rows and only those.
func TestRLSTenantSwitching(t *testing.T) {
	db := rlsDB(t)
	emailA := "switch-a@login.test"
	emailB := "switch-b@login.test"
	seedTenantUser(t, db, rlsTenantA, emailA)
	seedTenantUser(t, db, rlsTenantB, emailB)

	for _, tc := range []struct{ tenant, email string }{
		{rlsTenantA, emailA},
		{rlsTenantB, emailB},
		{rlsTenantA, emailA}, // switch back
	} {
		var got string
		err := tenant.WithTenant(t.Context(), db, tc.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `
				SELECT email FROM identity.users ORDER BY email LIMIT 1`).Scan(&got)
		})
		if err != nil {
			t.Fatalf("scoped read under %s: %v", tc.tenant, err)
		}
		if got != tc.email {
			t.Fatalf("tenant %s saw %q; want %q", tc.tenant, got, tc.email)
		}
	}
}

// The auth flows span tenants by design and run under the scoped bypass —
// login must keep working with RLS in force.
func TestLoginWorksUnderRLS(t *testing.T) {
	svc, db := testService(t)
	email := "rls-login@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, rlsTenantA, email, "s3cret!", "active", "admin")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|rls")
	if err != nil {
		t.Fatalf("login under RLS: %v", err)
	}
	if _, err := svc.Refresh(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("refresh under RLS: %v", err)
	}
	if err := svc.Logout(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("logout under RLS: %v", err)
	}
}
