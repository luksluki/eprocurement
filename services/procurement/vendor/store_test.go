package vendor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
)

func testStore(t *testing.T) *Store {
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
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return NewStore(db)
}

const (
	tenantA = "bbbbbbbb-0000-0000-0000-000000000001"
	tenantB = "bbbbbbbb-0000-0000-0000-000000000002"
	userA   = "bbbbbbbb-0000-0000-0000-0000000000aa"
)

func cleanupVendor(t *testing.T, db *pgxpool.Pool, tenantID, code string) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id = $1 AND code = $2`, tenantID, code)
		return err
	})
}

func createFor(t *testing.T, s *Store, tenantID, code string) Vendor {
	t.Helper()
	v, err := s.Create(t.Context(), tenantID, CreateInput{
		Code: code, Name: "PT Test Vendor", ContactEmail: "v@example.com", TaxID: "01.234.567.8-901", UserID: userA,
	})
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return v
}

// AC: duplicate code rejected — within one tenant.
func TestCreateDuplicateCodeRejected(t *testing.T) {
	s := testStore(t)
	t.Cleanup(func() { cleanupVendor(t, s.db, tenantA, "DUP-01") })

	createFor(t, s, tenantA, "DUP-01")

	_, err := s.Create(t.Context(), tenantA, CreateInput{Code: "DUP-01", Name: "PT Impostor", UserID: userA})
	if !errors.Is(err, ErrDuplicateCode) {
		t.Fatalf("err = %v, want ErrDuplicateCode", err)
	}

	// The same code in another tenant is fine — codes are tenant-scoped.
	other, err := s.Create(t.Context(), tenantB, CreateInput{Code: "DUP-01", Name: "CV Other Tenant", UserID: userA})
	if err != nil {
		t.Fatalf("same code in another tenant: %v", err)
	}
	t.Cleanup(func() { cleanupVendor(t, s.db, tenantB, "DUP-01") })
	if other.TenantID != tenantB || Normalize(other.Code) != "DUP-01" {
		t.Fatalf("vendor = %+v", other)
	}
}

// AC: responses include audit fields — created/updated timestamps and actors
// flow through create and update.
func TestAuditFieldsRoundTrip(t *testing.T) {
	s := testStore(t)
	t.Cleanup(func() { cleanupVendor(t, s.db, tenantA, "AUD-01") })

	v := createFor(t, s, tenantA, "AUD-01")
	if v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
		t.Fatalf("audit timestamps missing: %+v", v)
	}
	if v.CreatedBy != userA || v.UpdatedBy != userA {
		t.Fatalf("audit actors missing: created_by=%q updated_by=%q", v.CreatedBy, v.UpdatedBy)
	}

	userB := "bbbbbbbb-0000-0000-0000-0000000000bb"
	updated, err := s.Update(t.Context(), tenantA, UpdateInput{ID: v.ID, Name: "PT Renamed", UserID: userB})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.UpdatedBy != userB || updated.Name != "PT Renamed" {
		t.Fatalf("update did not stamp audit: %+v", updated)
	}
	if updated.CreatedBy != userA {
		t.Fatalf("created_by changed on update: %+v", updated)
	}
}

// AC: deactivated vendors cannot be used in new POs — GetForUse is the gate
// PO creation (#35) will call; deactivated vendors also become immutable.
func TestDeactivatedVendorUnusableAndImmutable(t *testing.T) {
	s := testStore(t)
	t.Cleanup(func() { cleanupVendor(t, s.db, tenantA, "DEA-01") })

	v := createFor(t, s, tenantA, "DEA-01")

	if _, err := s.GetForUse(t.Context(), tenantA, v.ID); err != nil {
		t.Fatalf("active vendor must be usable: %v", err)
	}

	deactivated, err := s.Deactivate(t.Context(), tenantA, v.ID, userA)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if deactivated.Status != "deactivated" {
		t.Fatalf("status = %s, want deactivated", deactivated.Status)
	}

	if _, err := s.GetForUse(t.Context(), tenantA, v.ID); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("GetForUse after deactivate: err = %v, want ErrDeactivated", err)
	}
	if _, err := s.Update(t.Context(), tenantA, UpdateInput{ID: v.ID, Name: "PT Zombie"}); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("update after deactivate: err = %v, want ErrDeactivated", err)
	}

	// Soft delete only: the row stays readable for history.
	got, err := s.Get(t.Context(), tenantA, v.ID)
	if err != nil || got.Status != "deactivated" {
		t.Fatalf("get after deactivate = (%+v, %v)", got, err)
	}

	// Deactivating again reports the deactivated state, not a crash.
	if _, err := s.Deactivate(t.Context(), tenantA, v.ID, userA); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("second deactivate: err = %v, want ErrDeactivated", err)
	}
}

func TestCrossTenantIsolation(t *testing.T) {
	s := testStore(t)
	t.Cleanup(func() { cleanupVendor(t, s.db, tenantA, "ISO-01") })

	v := createFor(t, s, tenantA, "ISO-01")

	// Tenant B cannot see tenant A's vendor — indistinguishable from missing.
	if _, err := s.Get(t.Context(), tenantB, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(t.Context(), tenantB, UpdateInput{ID: v.ID, Name: "PT Hijack"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Deactivate(t.Context(), tenantB, v.ID, userA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant deactivate: err = %v, want ErrNotFound", err)
	}
}

func TestValidation(t *testing.T) {
	// Codes are normalized (uppercased/trimmed) before validation — by the
	// gateway and again in the store — so these are the canonical forms.
	for _, tc := range []struct {
		code, name, email string
		wantErr           error
	}{
		{"", "PT Valid", "", ErrCodeInvalid},
		{"D", "PT Valid", "", ErrCodeInvalid},      // too short
		{"DUP_01", "PT Valid", "", ErrCodeInvalid}, // underscore rejected
		{"DUP 01", "PT Valid", "", ErrCodeInvalid}, // space rejected
		{"DUP-01", "", "", ErrNameInvalid},         // empty name
		{"DUP-01", "X", "", ErrNameInvalid},        // 1-char name
		{"DUP-01", "PT Valid", "not-an-email", ErrEmailInvalid},
		{"DUP-01", "PT Valid", "valid@example.com", nil},
		{"dup-01", "PT Valid", "", nil}, // normalization uppercases before validation
	} {
		if err := ValidateCreate(Normalize(tc.code), tc.name, tc.email); !errors.Is(err, tc.wantErr) {
			t.Errorf("ValidateCreate(%q, %q, %q) = %v, want %v", tc.code, tc.name, tc.email, err, tc.wantErr)
		}
	}
}

func TestNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.Get(t.Context(), tenantA, "cccccccc-0000-0000-0000-0000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
