package vendor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// Store persists vendors, always scoped to one tenant through the RLS
// helpers — there is deliberately no tenant-spanning path.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

const vendorColumns = `id, tenant_id, code, name, contact_email, tax_id, status,
	       created_at, created_by, updated_at, updated_by`

// CreateInput carries the create payload plus the acting user for audit.
type CreateInput struct {
	Code, Name, ContactEmail, TaxID, UserID string
}

// Create inserts a vendor for tenantID. A duplicate code in the same tenant
// returns ErrDuplicateCode.
func (s *Store) Create(ctx context.Context, tenantID string, in CreateInput) (Vendor, error) {
	in.Code = Normalize(in.Code)
	var v Vendor
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return scanVendor(tx.QueryRow(ctx, `
			INSERT INTO procurement.vendors (tenant_id, code, name, contact_email, tax_id, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, NULLIF($6, '')::uuid)
			RETURNING `+vendorColumns,
			tenantID, in.Code, in.Name, in.ContactEmail, in.TaxID, in.UserID), &v)
	})
	if err != nil {
		return Vendor{}, mapWriteError(err, "create")
	}
	return v, nil
}

// Get returns the vendor by id within tenantID; other tenants' vendors are
// indistinguishable from missing ones.
func (s *Store) Get(ctx context.Context, tenantID, id string) (Vendor, error) {
	var v Vendor
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return scanVendor(tx.QueryRow(ctx,
			`SELECT `+vendorColumns+` FROM procurement.vendors WHERE id = $1`, id), &v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Vendor{}, ErrNotFound
	}
	if err != nil {
		return Vendor{}, fmt.Errorf("vendor: get: %w", err)
	}
	return v, nil
}

// UpdateInput carries the mutable fields; empty strings keep stored values.
type UpdateInput struct {
	ID, Name, ContactEmail, TaxID, UserID string
}

// Update mutates an active vendor. Deactivated vendors are immutable
// (soft-deleted).
func (s *Store) Update(ctx context.Context, tenantID string, in UpdateInput) (Vendor, error) {
	v, err := s.update(ctx, tenantID, in)
	if errors.Is(err, pgx.ErrNoRows) {
		// Missing or already deactivated: distinguish for a correct code.
		if _, getErr := s.Get(ctx, tenantID, in.ID); errors.Is(getErr, ErrNotFound) {
			return Vendor{}, ErrNotFound
		} else if getErr == nil {
			return Vendor{}, ErrDeactivated
		} else {
			return Vendor{}, getErr
		}
	}
	return v, err
}

func (s *Store) update(ctx context.Context, tenantID string, in UpdateInput) (Vendor, error) {
	var v Vendor
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return scanVendor(tx.QueryRow(ctx, `
			UPDATE procurement.vendors SET
				name          = COALESCE(NULLIF($2, ''), name),
				contact_email = COALESCE(NULLIF($3, ''), contact_email),
				tax_id        = COALESCE(NULLIF($4, ''), tax_id),
				updated_at    = now(),
				updated_by    = NULLIF($5, '')::uuid
			WHERE id = $1 AND status = 'active'
			RETURNING `+vendorColumns,
			in.ID, in.Name, in.ContactEmail, in.TaxID, in.UserID), &v)
	})
	return v, err
}

// Deactivate soft-deletes the vendor: status flips to "deactivated" and the
// row becomes immutable but stays readable for history.
func (s *Store) Deactivate(ctx context.Context, tenantID, id, userID string) (Vendor, error) {
	v, err := s.deactivate(ctx, tenantID, id, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.Get(ctx, tenantID, id); errors.Is(getErr, ErrNotFound) {
			return Vendor{}, ErrNotFound
		} else if getErr == nil {
			return Vendor{}, ErrDeactivated // already soft-deleted
		} else {
			return Vendor{}, getErr
		}
	}
	return v, err
}

func (s *Store) deactivate(ctx context.Context, tenantID, id, userID string) (Vendor, error) {
	var v Vendor
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return scanVendor(tx.QueryRow(ctx, `
			UPDATE procurement.vendors
			SET status = 'deactivated', updated_at = now(), updated_by = NULLIF($2, '')::uuid
			WHERE id = $1 AND status = 'active'
			RETURNING `+vendorColumns, id, userID), &v)
	})
	return v, err
}

// GetForUse is the gate purchase-order creation (#35) must pass: only active
// vendors of the caller's tenant may be used in new POs.
func (s *Store) GetForUse(ctx context.Context, tenantID, id string) (Vendor, error) {
	v, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return Vendor{}, err
	}
	if v.Status != "active" {
		return Vendor{}, ErrDeactivated
	}
	return v, nil
}

// scanVendor reads one row; timestamps land as time.Time and nil audit ids
// become empty strings.
func scanVendor(row pgx.Row, v *Vendor) error {
	var createdBy, updatedBy *string
	err := row.Scan(&v.ID, &v.TenantID, &v.Code, &v.Name, &v.ContactEmail, &v.TaxID, &v.Status,
		&v.CreatedAt, &createdBy, &v.UpdatedAt, &updatedBy)
	if err != nil {
		return err
	}
	if createdBy != nil {
		v.CreatedBy = *createdBy
	}
	if updatedBy != nil {
		v.UpdatedBy = *updatedBy
	}
	return nil
}

func mapWriteError(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "vendors_tenant_code_uidx") {
		return ErrDuplicateCode
	}
	return fmt.Errorf("vendor: %s: %w", what, err)
}

// RFC3339 renders a timestamp for API responses.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
