// Store persists requisitions, always scoped to one tenant through the RLS
// helpers. The IO sentinels (machine-readable failure codes) live here, next
// to the domain's validation sentinels in requisition.go — the domain file
// stays pure and untouched. Number generation, the draft-only item edits, and
// the idempotent submit each serialize on the tenant's counter row / the
// requisition row inside one transaction.
package requisition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// IO failure codes; the gRPC API and the gateway carry them through to
// clients unchanged (same contract as the domain's validation sentinels).
var (
	ErrIDInvalid              = errors.New("requisition_id_invalid")
	ErrNotFound               = errors.New("requisition_not_found")
	ErrForbidden              = errors.New("requisition_forbidden")
	ErrIdempotencyKeyInvalid  = errors.New("requisition_idempotency_key_invalid")
	ErrIdempotencyKeyConflict = errors.New("requisition_idempotency_key_conflict")
)

// Store persists requisitions, always scoped to one tenant — there is
// deliberately no tenant-spanning path.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

const requisitionColumns = `id, tenant_id, number, requester_id, status, currency, total_cents,
		       submitted_at, created_at, created_by, updated_at, updated_by, submit_idempotency_key`

// CreateInput is the create payload: the requester (also the creator for
// audit) and the basket. Items may be empty — a draft collects its lines
// before submit. Currency must already be normalized (uppercased) by the API
// layer; the domain matches exactly.
type CreateInput struct {
	RequesterID string
	Currency    string
	Items       []Item
}

// Create inserts a draft requisition with a server-generated, tenant-scoped
// number. The number counter is lazily initialized from the tenant's highest
// existing PR number and incremented under its row lock inside this insert
// transaction, so concurrent creates in one tenant get distinct sequential
// numbers. Domain validation failures (currency, items) roll the whole
// transaction back, including the counter increment.
func (s *Store) Create(ctx context.Context, tenantID string, in CreateInput) (Requisition, error) {
	var out Requisition
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		number, err := nextNumber(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		agg, err := New(NewInput{
			TenantID: tenantID, Number: number, RequesterID: in.RequesterID,
			Currency: in.Currency, Items: in.Items,
			Now: time.Now().UTC(), CreatedBy: in.RequesterID,
		})
		if err != nil {
			return err
		}
		out, err = insertRequisition(ctx, tx, agg)
		return err
	})
	if err != nil {
		return Requisition{}, wrap("create", err)
	}
	return out, nil
}

// nextNumber allocates the next PR-%06d number for tenantID inside tx. The
// counter row is lazily initialized from the tenant's highest existing
// number (INSERT ... ON CONFLICT DO NOTHING — concurrent initializers race
// harmlessly, one wins) and then incremented with UPDATE ... RETURNING: the
// row lock serializes all allocators, so numbers are unique and sequential
// per tenant.
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO procurement.requisition_counters (tenant_id, last_number)
		SELECT $1, coalesce(max((substring(number FROM 4))::bigint), 0)
		FROM procurement.requisitions
		WHERE tenant_id = $1 AND number ~ '^PR-[0-9]+$'
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID); err != nil {
		return "", fmt.Errorf("counter init: %w", err)
	}
	var n int64
	if err := tx.QueryRow(ctx, `
		UPDATE procurement.requisition_counters
		SET last_number = last_number + 1
		WHERE tenant_id = $1
		RETURNING last_number`, tenantID).Scan(&n); err != nil {
		return "", fmt.Errorf("counter next: %w", err)
	}
	return fmt.Sprintf("PR-%06d", n), nil
}

func insertRequisition(ctx context.Context, tx pgx.Tx, agg *Requisition) (Requisition, error) {
	var out Requisition
	if _, err := scanRequisition(tx.QueryRow(ctx, `
		INSERT INTO procurement.requisitions
			(tenant_id, number, requester_id, status, currency, total_cents, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, NULLIF($7, '')::uuid)
		RETURNING `+requisitionColumns,
		agg.TenantID, agg.Number, agg.RequesterID, agg.Status, agg.Currency, agg.TotalCents,
		agg.CreatedBy), &out); err != nil {
		return Requisition{}, err
	}
	if err := insertItems(ctx, tx, out.ID, agg.Items); err != nil {
		return Requisition{}, err
	}
	out.Items = append([]Item(nil), agg.Items...)
	return out, nil
}

func insertItems(ctx context.Context, tx pgx.Tx, requisitionID string, items []Item) error {
	for _, it := range items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO procurement.requisition_items (requisition_id, description, qty, unit_price_cents, category)
			VALUES ($1, $2, $3, $4, $5)`,
			requisitionID, it.Description, it.Qty, it.UnitPriceCents, it.Category); err != nil {
			return err
		}
	}
	return nil
}

// Get returns one requisition of tenantID (header plus items) without
// locking; other tenants' requisitions are indistinguishable from missing
// ones. Non-UUID ids fail fast with ErrIDInvalid instead of a database cast
// error.
func (s *Store) Get(ctx context.Context, tenantID, id string) (Requisition, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Requisition{}, ErrIDInvalid
	}
	var out Requisition
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		stored, err := loadForUpdate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = stored.Requisition
		return nil
	})
	if err != nil {
		return Requisition{}, wrap("get", err)
	}
	return out, nil
}

// UpdateItems replaces the whole basket of a draft and recomputes the total
// (the edit API). The requisition row is loaded FOR UPDATE first — a
// concurrent submit on the same requisition serializes behind the same lock —
// then mutated under a status='draft' guard.
func (s *Store) UpdateItems(ctx context.Context, tenantID, id string, items []Item, actor string) (Requisition, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Requisition{}, ErrIDInvalid
	}
	var out Requisition
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		stored, err := loadForUpdate(ctx, tx, id, true)
		if err != nil {
			return err
		}
		agg := stored.Requisition
		if err := agg.UpdateItems(items, time.Now().UTC(), actor); err != nil {
			return err
		}
		var updated Requisition
		if _, err := scanRequisition(tx.QueryRow(ctx, `
			UPDATE procurement.requisitions
			SET total_cents = $2, updated_at = now(), updated_by = NULLIF($3, '')::uuid
			WHERE id = $1 AND status = 'draft'
			RETURNING `+requisitionColumns,
			id, agg.TotalCents, actor), &updated); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrIllegalTransition // lost the draft race despite the lock
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM procurement.requisition_items WHERE requisition_id = $1`, id); err != nil {
			return err
		}
		if err := insertItems(ctx, tx, id, agg.Items); err != nil {
			return err
		}
		updated.Items = append([]Item(nil), agg.Items...)
		out = updated
		return nil
	})
	if err != nil {
		return Requisition{}, wrap("update items", err)
	}
	return out, nil
}

// Submit moves a draft to submitted. The row lock (FOR UPDATE) serializes a
// concurrent edit/submit pair on the same requisition; the replay check runs
// after the lock, so a replayed Idempotency-Key deterministically returns the
// requisition's current state instead of racing the original submit. The key
// is recorded in the same transaction as the transition; a key already used
// by a different requisition of the tenant trips the partial unique index
// (23505 → ErrIdempotencyKeyConflict).
//
// materialize runs inside the same transaction right after the successful
// draft → submitted UPDATE (never on a replay): the approval engine (#30)
// evaluates the tenant's rules and inserts the materialized approval steps
// here, so a submit without applicable rules (approval.ErrRulesMissing) rolls
// the whole submit back — status stays draft and the idempotency key is not
// recorded. It is nil in tests that exercise the requisition store alone.
func (s *Store) Submit(ctx context.Context, tenantID, id, actor, idempotencyKey string, materialize func(tx pgx.Tx, r Requisition) error) (Requisition, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Requisition{}, ErrIDInvalid
	}
	var out Requisition
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		stored, err := loadForUpdate(ctx, tx, id, true)
		if err != nil {
			return err
		}
		// Replay: the same key already named this requisition's submit.
		// Return the current state as-is (idempotent success), whatever the
		// requisition has moved on to since — the contract is pinned in
		// docs/schema/procurement.md.
		if idempotencyKey != "" && stored.submitKey == idempotencyKey {
			out = stored.Requisition
			return nil
		}
		agg := stored.Requisition
		if err := agg.Submit(time.Now().UTC(), actor); err != nil {
			return err
		}
		var updated Requisition
		if _, err := scanRequisition(tx.QueryRow(ctx, `
			UPDATE procurement.requisitions
			SET status = 'submitted', submitted_at = now(), updated_at = now(),
			    updated_by = NULLIF($2, '')::uuid, submit_idempotency_key = NULLIF($3, '')
			WHERE id = $1 AND status = 'draft'
			RETURNING `+requisitionColumns,
			id, actor, idempotencyKey), &updated); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrIllegalTransition // lost the draft race despite the lock
			}
			return mapSubmitWriteError(err)
		}
		if materialize != nil {
			if err := materialize(tx, updated); err != nil {
				return err // rolls the whole submit back with the steps
			}
		}
		updated.Items = append([]Item(nil), agg.Items...)
		out = updated
		return nil
	})
	if err != nil {
		return Requisition{}, wrap("submit", err)
	}
	return out, nil
}

// LoadForUpdate loads one requisition (header plus items) inside tx under
// the row lock. Exported for the approval store (#30), whose Decide runs in
// a transaction that serializes on the same requisition row lock as
// submit/edit; RLS makes other tenants' rows invisible (ErrNotFound).
func LoadForUpdate(ctx context.Context, tx pgx.Tx, id string) (Requisition, error) {
	stored, err := loadForUpdate(ctx, tx, id, true)
	return stored.Requisition, err
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels (domain validation and IO) through bare, so err.Error() carries
// the code verbatim through gRPC — same contract as the vendor store.
func wrap(what string, err error) error {
	for _, sentinel := range []error{
		ErrCurrencyInvalid, ErrDescriptionInvalid, ErrQtyInvalid,
		ErrUnitPriceInvalid, ErrItemsEmpty, ErrIllegalTransition,
		ErrNotFound, ErrIDInvalid, ErrForbidden,
		ErrIdempotencyKeyInvalid, ErrIdempotencyKeyConflict,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("requisition: %s: %w", what, err)
}

// storedRequisition couples the domain aggregate with the storage-only
// submit idempotency key (the domain deliberately has no such field).
type storedRequisition struct {
	Requisition
	submitKey string
}

// loadForUpdate loads one requisition (header plus items) inside tx, taking
// the row lock when forWrite is set. RLS makes other tenants' rows invisible,
// so they surface as ErrNotFound.
func loadForUpdate(ctx context.Context, tx pgx.Tx, id string, forWrite bool) (storedRequisition, error) {
	var stored storedRequisition
	query := `SELECT ` + requisitionColumns + ` FROM procurement.requisitions WHERE id = $1`
	if forWrite {
		query += ` FOR UPDATE`
	}
	key, err := scanRequisition(tx.QueryRow(ctx, query, id), &stored.Requisition)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storedRequisition{}, ErrNotFound
		}
		return storedRequisition{}, err
	}
	stored.submitKey = key
	items, err := loadItems(ctx, tx, id)
	if err != nil {
		return storedRequisition{}, err
	}
	stored.Items = items
	return stored, nil
}

// scanRequisition reads one header row; timestamps land as time.Time and nil
// audit ids become empty strings. The submit idempotency key is returned as a
// value (empty when NULL).
func scanRequisition(row pgx.Row, r *Requisition) (submitKey string, err error) {
	var key *string
	var createdBy, updatedBy *string
	err = row.Scan(&r.ID, &r.TenantID, &r.Number, &r.RequesterID, &r.Status,
		&r.Currency, &r.TotalCents, &r.SubmittedAt, &r.CreatedAt,
		&createdBy, &r.UpdatedAt, &updatedBy, &key)
	if err != nil {
		return "", err
	}
	if createdBy != nil {
		r.CreatedBy = *createdBy
	}
	if updatedBy != nil {
		r.UpdatedBy = *updatedBy
	}
	if key != nil {
		submitKey = *key
	}
	return submitKey, nil
}

func loadItems(ctx context.Context, tx pgx.Tx, requisitionID string) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT description, qty, unit_price_cents, category
		FROM procurement.requisition_items
		WHERE requisition_id = $1
		ORDER BY created_at, id`, requisitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.Description, &it.Qty, &it.UnitPriceCents, &it.Category); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// listPredicates is the shared WHERE clause of the list's page subquery and
// its count arm.
const listPredicates = `WHERE ($1::text = '' OR status = $1::text)
			  AND ($2::text = '' OR number LIKE $2::text || '%')`

// List returns one page of requisition headers ordered by number descending
// plus the total number of matching rows (across pages). Filters: status
// equality and a number-prefix query — escaped so user input with % or _ is
// literal. The filter must come from ValidateList. Rows are header-only:
// Items and approval steps are not loaded (the detail reads own them).
//
// The total rides the same single statement as the page: an exact COUNT as
// an uncorrelated scalar subquery, which the executor evaluates once as an
// InitPlan regardless of join strategy. #27's vendor list uses count(*)
// OVER() instead; here the window's tuplestore spilled to disk at 100k rows
// under the default work_mem, and a CROSS JOIN count arm was re-executed per
// page row by the planner (loops=20, measured during #32 — see
// docs/schema/procurement.md). The subquery form has neither problem. An
// empty page (past the end) emits no rows, so an explicit COUNT fallback
// recovers the true total — the same contract as the vendor list.
func (s *Store) List(ctx context.Context, tenantID string, f ListFilter) ([]Requisition, int, error) {
	var (
		requisitions []Requisition
		total        int
	)
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		offset := (int(f.Page) - 1) * int(f.PageSize)
		// The page arm sorts with COLLATE "C": requisition numbers are
		// canonical server-generated ASCII (PR-%06d), so C ordering equals
		// the default collation for every storable value while comparisons
		// stay memcmp-cheap instead of locale function calls — measured 4×
		// faster on a libc-collation platform at 100k rows (#32).
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.tenant_id, c.number, c.requester_id, c.status, c.currency, c.total_cents,
			       c.submitted_at, c.created_at, c.created_by, c.updated_at, c.updated_by,
			       c.submit_idempotency_key,
			       (SELECT count(*) FROM procurement.requisitions `+listPredicates+`) AS total_count
			FROM (SELECT `+requisitionColumns+`
			      FROM procurement.requisitions
			      `+listPredicates+`
			      ORDER BY number COLLATE "C" DESC
			      LIMIT $3 OFFSET $4) c
			ORDER BY c.number DESC`,
			f.Status, f.Query, f.PageSize, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				r                    Requisition
				submitKey            *string // discarded: the list is header-only
				createdBy, updatedBy *string
			)
			if err := rows.Scan(&r.ID, &r.TenantID, &r.Number, &r.RequesterID, &r.Status,
				&r.Currency, &r.TotalCents, &r.SubmittedAt, &r.CreatedAt,
				&createdBy, &r.UpdatedAt, &updatedBy, &submitKey, &total); err != nil {
				return err
			}
			if createdBy != nil {
				r.CreatedBy = *createdBy
			}
			if updatedBy != nil {
				r.UpdatedBy = *updatedBy
			}
			requisitions = append(requisitions, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(requisitions) == 0 {
			// Past-the-end page: the join's left side is empty; count matches
			// explicitly so callers still see the real total.
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM procurement.requisitions `+listPredicates, f.Status, f.Query).Scan(&total)
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("requisition: list: %w", err)
	}
	return requisitions, total, nil
}

// mapSubmitWriteError recognizes the partial-unique-index violation on the
// submit idempotency key; everything else passes through.
func mapSubmitWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "requisitions_tenant_submit_key_uidx" {
		return ErrIdempotencyKeyConflict
	}
	return err
}

// RFC3339 renders a timestamp for API responses.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
