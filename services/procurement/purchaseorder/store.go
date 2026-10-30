// Store persists purchase orders, always scoped to one tenant through the
// RLS helpers — there is deliberately no tenant-spanning path. The
// conversion (CreateFromRequisition) runs in one transaction whose step order
// is pinned; every step either commits with the PO, its items, the closed
// requisition, and exactly one po.created outbox row — or nothing lands.
package purchaseorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/outbox"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// Store persists purchase orders, always scoped to one tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// IO failure codes; the gRPC API and the gateway carry them through to
// clients unchanged (same contract as the domain's validation sentinels —
// the requisition store's split).
var (
	ErrIDInvalid = errors.New("po_id_invalid")
	ErrNotFound  = errors.New("po_not_found")
)

const poColumns = `id, tenant_id, number, requisition_id, vendor_id, status, currency,
	       total_cents, issued_at, created_at, created_by, updated_at, updated_by`

// OutboxCreated is the outbox event a successful conversion writes:
// "po.created". Two segments so the PRD §14.5 po.* topic bindings deliver it
// (the * wildcard matches exactly one word). It extends the §14.6 catalog —
// the catalog's first PO event is po.issued, which #36 writes on the issue
// transition — the same documented extension pattern as #31's
// pr.step_approved. Pinned in docs/schema/procurement.md.
const OutboxCreated = "po.created"

// CreatedPayload is the JSONB payload of the po.created outbox event. The
// fields are pinned (docs/schema/procurement.md); requisition_number lets a
// PO timeline link back to its requisition (#43), the rest is the minimum
// the notification consumer (#45) and projections need.
type CreatedPayload struct {
	Number            string `json:"number"`
	RequisitionID     string `json:"requisition_id"`
	RequisitionNumber string `json:"requisition_number"`
	VendorID          string `json:"vendor_id"`
	TotalCents        int64  `json:"total_cents"`
	Currency          string `json:"currency"`
	CreatedBy         string `json:"created_by"`
}

// CreateFromRequisition converts one approved requisition into a draft PO
// (FR-05). The whole flow is one tenant transaction with a pinned order:
//
//  1. lock the requisition row (requisition.LoadForUpdate — the same
//     serialization point as submit/edit/decide, so a concurrent decide or a
//     second conversion of this requisition blocks here);
//  2. replay lookup by (tenant_id, requisition_id): a PO that already exists
//     for this requisition is returned as-is with created=false — the vendor
//     gate is skipped, a differing vendor_id is ignored, nothing is written
//     ("cannot convert twice" is a replay, not an error; the unique index
//     backs it up);
//  3. vendor gate inside this transaction (vendor.ForUseTx — the tx-scoped
//     twin of GetForUse): only an active vendor of this tenant converts, and
//     it cannot be deactivated between the check and the insert;
//  4. status gate: only an approved requisition converts;
//  5. allocate the next PO-%06d number from po_counters (UPDATE … RETURNING
//     under the row lock) — after the gates, so a failed conversion rolls
//     the increment back and burns no number;
//  6. guarded close of the requisition (approved → closed through the
//     domain edge the #28 state machine reserved for this flow);
//  7. insert the PO and copy the requisition lines exactly once, recomputing
//     total_cents server-side (equal to the requisition's total by
//     construction — asserted);
//  8. append exactly one po.created outbox row — the same transaction, so
//     the event and the PO commit atomically (no dual writes, ADR-0001).
//
// The created return separates a fresh conversion (true) from a replay
// (false): the REST edge answers 201 vs 200 on it. Cross-tenant
// requisition or vendor ids are indistinguishable from missing ones (404);
// non-UUID ids fail fast before the transaction opens.
func (s *Store) CreateFromRequisition(ctx context.Context, tenantID, requisitionID, vendorID, actor string) (PurchaseOrder, bool, error) {
	if _, err := uuid.Parse(requisitionID); err != nil {
		return PurchaseOrder{}, false, requisition.ErrIDInvalid
	}
	vendorID = strings.TrimSpace(vendorID)
	if _, err := uuid.Parse(vendorID); err != nil {
		return PurchaseOrder{}, false, ErrVendorIDInvalid
	}
	var out PurchaseOrder
	var created bool
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Step 1: lock the requisition — the shared serialization point.
		req, err := requisition.LoadForUpdate(ctx, tx, requisitionID)
		if err != nil {
			return err
		}
		// Step 2: replay — this requisition was already converted; return
		// the existing PO untouched (vendor gate skipped, vendor_id ignored).
		existing, found, err := loadByRequisition(ctx, tx, tenantID, requisitionID)
		if err != nil {
			return err
		}
		if found {
			out = existing
			return nil
		}
		// Step 3: vendor gate on this transaction (anti-TOCTOU).
		if _, err := vendor.ForUseTx(ctx, tx, vendorID); err != nil {
			return err
		}
		// Step 4: status gate.
		if req.Status != requisition.StatusApproved {
			return ErrRequisitionNotApproved
		}
		// Step 5: number allocation — inside the transaction, after the
		// gates, so failures roll the increment back.
		number, err := nextNumber(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		agg, err := New(NewInput{
			TenantID: tenantID, Number: number,
			RequisitionID: req.ID, VendorID: vendorID,
			Currency: req.Currency, Items: itemsOf(req.Items),
			Now: now, CreatedBy: actor,
		})
		if err != nil {
			return err
		}
		// The copy invariant: the recomputed total equals the requisition's.
		if agg.TotalCents != req.TotalCents {
			return fmt.Errorf("po conversion: copied total %d != requisition total %d", agg.TotalCents, req.TotalCents)
		}
		// Step 6: the domain-owned close edge (approved → closed); the
		// guarded write re-checks the status the lock already protects.
		if err := req.Close(now, actor); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE procurement.requisitions
			SET status = 'closed', updated_at = now(), updated_by = NULLIF($2, '')::uuid
			WHERE id = $1 AND status = 'approved'`,
			req.ID, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrRequisitionNotApproved // lost the race despite the lock
		}
		// Step 7: insert the PO and its copied lines.
		stored, err := insertPO(ctx, tx, agg)
		if err != nil {
			return err
		}
		// Step 8: exactly one outbox row, committed with the PO.
		if err := outbox.AppendTx(ctx, tx, outbox.Event{
			ID: uuid.NewString(), TenantID: tenantID, EventType: OutboxCreated,
			Version: 1,
			Payload: CreatedPayload{
				Number:            stored.Number,
				RequisitionID:     req.ID,
				RequisitionNumber: req.Number,
				VendorID:          stored.VendorID,
				TotalCents:        stored.TotalCents,
				Currency:          stored.Currency,
				CreatedBy:         actor,
			},
			ActorID:    actor,
			ActorIP:    logger.ActorIP(ctx),
			TraceID:    logger.TraceID(ctx),
			EntityType: "purchase_order",
			EntityID:   stored.ID,
			OccurredAt: now,
		}); err != nil {
			return err
		}
		out = stored
		created = true
		return nil
	})
	if err != nil {
		return PurchaseOrder{}, false, wrap("create from requisition", err)
	}
	return out, created, nil
}

// Lifecycle outbox event types (#36). po.issued and po.acknowledged are PRD
// §14.6 catalog entries; po.cancelled and po.closed extend the catalog the
// same way #35's po.created and #31's pr.step_approved did — two segments so
// the §14.5 po.* topic bindings (wildcard = exactly one word) deliver them.
// Pinned with their payloads in docs/schema/procurement.md.
const (
	OutboxIssued       = "po.issued"
	OutboxAcknowledged = "po.acknowledged"
	OutboxCancelled    = "po.cancelled"
	OutboxClosed       = "po.closed"
)

// TransitionPayload is the JSONB payload shared by the four lifecycle outbox
// events: the identifying fields a notification consumer (#45) or timeline
// projection (#43) needs, plus the new status and who performed the action
// (the envelope's actor_id column carries the same user; performed_by keeps
// the payload self-contained, the DecisionPayload decided_by precedent).
type TransitionPayload struct {
	Number        string `json:"number"`
	RequisitionID string `json:"requisition_id"`
	VendorID      string `json:"vendor_id"`
	Status        string `json:"status"`
	TotalCents    int64  `json:"total_cents"`
	Currency      string `json:"currency"`
	PerformedBy   string `json:"performed_by"`
}

// transition applies one lifecycle action (issue/acknowledge/cancel/close)
// in a single tenant transaction whose step order is pinned:
//
//  1. load the PO header + items under the row lock (FOR UPDATE) — the
//     serialization point every lifecycle transition shares, so concurrent
//     transitions on one PO (and #37's receipts, which will take the same
//     lock) serialize here: exactly one lands, the rest fail cleanly;
//  2. run the domain method — edge guard, cancel receipt guard, one
//     aggregate event;
//  3. guarded UPDATE re-checking the from-status the lock already protects
//     (belt and braces — losing that race is ErrIllegalTransition);
//  4. append exactly one outbox row (eventType), committed with the UPDATE:
//     an illegal transition writes no row because the transaction rolls
//     back before step 4 (ADR-0001 — no dual writes).
//
// issued_at is written as the post-transition aggregate value: now on issue,
// the previously stamped time on acknowledge/cancel/close, nil only for a
// draft cancel. Cross-tenant ids are indistinguishable from missing ones
// (ErrNotFound); non-UUID ids fail fast before the transaction opens.
func (s *Store) transition(ctx context.Context, tenantID, id, actor, what, eventType string,
	apply func(*PurchaseOrder, time.Time, string) error) (PurchaseOrder, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PurchaseOrder{}, ErrIDInvalid
	}
	var out PurchaseOrder
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Step 1: lock the PO — the shared serialization point.
		po, err := LoadForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		// Step 2: the domain owns the guards and the aggregate event.
		from := po.Status
		now := time.Now().UTC()
		if err := apply(&po, now, actor); err != nil {
			return err
		}
		// Step 3: the guarded write re-checks the from-status.
		var updated PurchaseOrder
		if err := scanPO(tx.QueryRow(ctx, `
			UPDATE procurement.purchase_orders
			SET status = $2, issued_at = $3, updated_at = now(), updated_by = NULLIF($4, '')::uuid
			WHERE id = $1 AND status = $5
			RETURNING `+poColumns,
			id, po.Status, po.IssuedAt, actor, from), &updated); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrIllegalTransition // lost the race despite the lock
			}
			return err
		}
		// Step 4: exactly one outbox row, committed with the transition.
		if err := outbox.AppendTx(ctx, tx, outbox.Event{
			ID: uuid.NewString(), TenantID: tenantID, EventType: eventType,
			Version: 1,
			Payload: TransitionPayload{
				Number: updated.Number, RequisitionID: updated.RequisitionID,
				VendorID: updated.VendorID, Status: updated.Status,
				TotalCents: updated.TotalCents, Currency: updated.Currency,
				PerformedBy: actor,
			},
			ActorID:    actor,
			ActorIP:    logger.ActorIP(ctx),
			TraceID:    logger.TraceID(ctx),
			EntityType: "purchase_order",
			EntityID:   updated.ID,
			OccurredAt: now,
		}); err != nil {
			return err
		}
		updated.Items = po.Items
		out = updated
		return nil
	})
	if err != nil {
		return PurchaseOrder{}, wrap(what, err)
	}
	return out, nil
}

// Issue moves a draft PO to issued and stamps issued_at, committing exactly
// one po.issued row (the §14.6 catalog event) with the transition. Anything
// but draft → ErrIllegalTransition (HTTP 409).
func (s *Store) Issue(ctx context.Context, tenantID, id, actor string) (PurchaseOrder, error) {
	return s.transition(ctx, tenantID, id, actor, "issue", OutboxIssued, (*PurchaseOrder).Issue)
}

// Acknowledge moves an issued PO to acknowledged — the vendor's confirmation
// (PRD SLA §16) — committing exactly one po.acknowledged row. Anything but
// issued → ErrIllegalTransition.
func (s *Store) Acknowledge(ctx context.Context, tenantID, id, actor string) (PurchaseOrder, error) {
	return s.transition(ctx, tenantID, id, actor, "acknowledge", OutboxAcknowledged, (*PurchaseOrder).Acknowledge)
}

// Cancel moves a draft/issued/acknowledged PO to cancelled — but only while
// the total qty_received over all lines is 0; receipts pin the PO and the
// only exit is Close (ErrHasReceipts → HTTP 409 po_has_receipts). Commits
// exactly one po.cancelled row on success.
func (s *Store) Cancel(ctx context.Context, tenantID, id, actor string) (PurchaseOrder, error) {
	return s.transition(ctx, tenantID, id, actor, "cancel", OutboxCancelled, (*PurchaseOrder).Cancel)
}

// Close moves a partially_received (short-close) or received PO to closed,
// committing exactly one po.closed row. Anything else → ErrIllegalTransition.
func (s *Store) Close(ctx context.Context, tenantID, id, actor string) (PurchaseOrder, error) {
	return s.transition(ctx, tenantID, id, actor, "close", OutboxClosed, (*PurchaseOrder).Close)
}

// LoadForUpdate loads one PO (header plus items) inside tx under the row
// lock — the lifecycle serialization point, exported for the goods-receipt
// store (#37) whose posts share it with every lifecycle transition (the
// requisition.LoadForUpdate precedent, #30). RLS makes other tenants' rows
// invisible, so they surface as ErrNotFound.
func LoadForUpdate(ctx context.Context, tx pgx.Tx, id string) (PurchaseOrder, error) {
	var po PurchaseOrder
	if err := scanPO(tx.QueryRow(ctx, `
		SELECT `+poColumns+` FROM procurement.purchase_orders
		WHERE id = $1 FOR UPDATE`, id), &po); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PurchaseOrder{}, ErrNotFound
		}
		return PurchaseOrder{}, err
	}
	items, err := loadItems(ctx, tx, po.ID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	po.Items = items
	return po, nil
}

// Get returns one PO of tenantID (header plus items) without locking — the
// detail read behind #38's list/detail UI (the requisition Get precedent:
// cross-tenant reads are indistinguishable from missing via RLS). Non-UUID
// ids fail fast with ErrIDInvalid instead of a database cast error.
func (s *Store) Get(ctx context.Context, tenantID, id string) (PurchaseOrder, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PurchaseOrder{}, ErrIDInvalid
	}
	var out PurchaseOrder
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		if err := scanPO(tx.QueryRow(ctx, `
			SELECT `+poColumns+` FROM procurement.purchase_orders
			WHERE id = $1`, id), &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		items, err := loadItems(ctx, tx, out.ID)
		if err != nil {
			return err
		}
		out.Items = items
		return nil
	})
	if err != nil {
		return PurchaseOrder{}, wrap("get", err)
	}
	return out, nil
}

// listPredicates is the shared WHERE clause of the list's page subquery and
// its count arm (#38, the #32 requisition-list contract).
const listPredicates = `WHERE ($1::text = '' OR status = $1::text)
			  AND ($2::text = '' OR number LIKE $2::text || '%')`

// List returns one page of PO headers ordered by number descending (newest
// first — numbers are sequential per tenant) plus the total number of
// matching rows (across pages). Filters: status equality and a number-prefix
// query — escaped so user input with % or _ is literal. The filter must come
// from ValidateList. Rows are header-only: Items are not loaded (the detail
// read owns them).
//
// The total rides the same single statement as the page: an exact COUNT as
// an uncorrelated scalar subquery (an executor InitPlan — evaluated exactly
// once), attached to the LIMIT/OFFSET page subquery. This is the #32 shape,
// adopted on its measured evidence: the #27 `count(*) OVER()` window
// tuplestore spilled to temp files at 100k rows under the default work_mem,
// and a CROSS JOIN count arm was re-executed per page row by the planner —
// the scalar subquery has neither problem. The page arm sorts with
// `number COLLATE "C" DESC`: every storable PO number is canonical
// server-generated ASCII (PO-%06d), so C ordering equals the default
// collation for this data while comparisons stay memcmp-cheap. An empty page
// (past the end) emits no rows, so an explicit COUNT fallback recovers the
// true total — the same contract as the requisition list.
func (s *Store) List(ctx context.Context, tenantID string, f ListFilter) ([]PurchaseOrder, int, error) {
	var (
		pos   []PurchaseOrder
		total int
	)
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		offset := (int(f.Page) - 1) * int(f.PageSize)
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.tenant_id, c.number, c.requisition_id, c.vendor_id, c.status, c.currency,
			       c.total_cents, c.issued_at, c.created_at, c.created_by, c.updated_at, c.updated_by,
			       (SELECT count(*) FROM procurement.purchase_orders `+listPredicates+`) AS total_count
			FROM (SELECT `+poColumns+`
			      FROM procurement.purchase_orders
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
				po                   PurchaseOrder
				createdBy, updatedBy *string
			)
			if err := rows.Scan(&po.ID, &po.TenantID, &po.Number, &po.RequisitionID,
				&po.VendorID, &po.Status, &po.Currency, &po.TotalCents, &po.IssuedAt,
				&po.CreatedAt, &createdBy, &po.UpdatedAt, &updatedBy, &total); err != nil {
				return err
			}
			if createdBy != nil {
				po.CreatedBy = *createdBy
			}
			if updatedBy != nil {
				po.UpdatedBy = *updatedBy
			}
			pos = append(pos, po)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(pos) == 0 {
			// Past-the-end page: the join's left side is empty; count matches
			// explicitly so callers still see the real total.
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM procurement.purchase_orders `+listPredicates, f.Status, f.Query).Scan(&total)
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("purchaseorder: list: %w", err)
	}
	return pos, total, nil
}

// nextNumber allocates the next PO-%06d number for tenantID inside tx, the
// same counter pattern as requisition numbers (0004): the row is initialized
// lazily from the tenant's highest existing number (INSERT … ON CONFLICT DO
// NOTHING), then UPDATE … RETURNING takes the row lock that serializes all
// allocators of the tenant. The increment rolls back with a failed
// conversion, so the per-tenant sequence is gap-free.
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO procurement.po_counters (tenant_id, last_number)
		SELECT $1, coalesce(max((substring(number FROM 4))::bigint), 0)
		FROM procurement.purchase_orders
		WHERE tenant_id = $1 AND number ~ '^PO-[0-9]+$'
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID); err != nil {
		return "", fmt.Errorf("counter init: %w", err)
	}
	var n int64
	if err := tx.QueryRow(ctx, `
		UPDATE procurement.po_counters
		SET last_number = last_number + 1
		WHERE tenant_id = $1
		RETURNING last_number`, tenantID).Scan(&n); err != nil {
		return "", fmt.Errorf("counter next: %w", err)
	}
	return fmt.Sprintf("PO-%06d", n), nil
}

// loadByRequisition returns the PO a tenant already created for this
// requisition, if any — the replay lookup of step 2.
func loadByRequisition(ctx context.Context, tx pgx.Tx, tenantID, requisitionID string) (PurchaseOrder, bool, error) {
	var po PurchaseOrder
	if err := scanPO(tx.QueryRow(ctx, `
		SELECT `+poColumns+` FROM procurement.purchase_orders
		WHERE tenant_id = $1 AND requisition_id = $2`, tenantID, requisitionID), &po); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PurchaseOrder{}, false, nil
		}
		return PurchaseOrder{}, false, err
	}
	items, err := loadItems(ctx, tx, po.ID)
	if err != nil {
		return PurchaseOrder{}, false, err
	}
	po.Items = items
	return po, true, nil
}

func insertPO(ctx context.Context, tx pgx.Tx, agg *PurchaseOrder) (PurchaseOrder, error) {
	var out PurchaseOrder
	if err := scanPO(tx.QueryRow(ctx, `
		INSERT INTO procurement.purchase_orders
			(tenant_id, number, requisition_id, vendor_id, status, currency, total_cents, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid, NULLIF($8, '')::uuid)
		RETURNING `+poColumns,
		agg.TenantID, agg.Number, agg.RequisitionID, agg.VendorID, agg.Status,
		agg.Currency, agg.TotalCents, agg.CreatedBy), &out); err != nil {
		return PurchaseOrder{}, err
	}
	for _, it := range agg.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO procurement.po_items (po_id, description, qty_ordered, unit_price_cents)
			VALUES ($1, $2, $3, $4)`,
			out.ID, it.Description, it.Qty, it.UnitPriceCents); err != nil {
			return PurchaseOrder{}, err
		}
	}
	out.Items = append([]Item(nil), agg.Items...)
	return out, nil
}

func loadItems(ctx context.Context, tx pgx.Tx, poID string) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, description, qty_ordered, qty_received, unit_price_cents
		FROM procurement.po_items
		WHERE po_id = $1
		ORDER BY created_at, id`, poID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Description, &it.Qty, &it.QtyReceived, &it.UnitPriceCents); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// scanPO reads one header row; timestamps land as time.Time, issued_at as a
// nilable pointer (NULL while draft), and nil audit ids become empty
// strings.
func scanPO(row pgx.Row, po *PurchaseOrder) error {
	var createdBy, updatedBy *string
	if err := row.Scan(&po.ID, &po.TenantID, &po.Number, &po.RequisitionID,
		&po.VendorID, &po.Status, &po.Currency, &po.TotalCents, &po.IssuedAt,
		&po.CreatedAt, &createdBy, &po.UpdatedAt, &updatedBy); err != nil {
		return err
	}
	if createdBy != nil {
		po.CreatedBy = *createdBy
	}
	if updatedBy != nil {
		po.UpdatedBy = *updatedBy
	}
	return nil
}

// itemsOf converts the requisition's lines to PO lines — the verbatim copy
// (qty_received starts at 0; #37 advances it).
func itemsOf(reqItems []requisition.Item) []Item {
	items := make([]Item, 0, len(reqItems))
	for _, it := range reqItems {
		items = append(items, Item{
			Description: it.Description, Qty: it.Qty, UnitPriceCents: it.UnitPriceCents,
		})
	}
	return items
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels (domain validation, the lifecycle guards, IO, and the
// requisition/vendor gates this package crosses) through bare, so
// err.Error() carries the code verbatim through gRPC — same contract as the
// requisition/vendor stores.
func wrap(what string, err error) error {
	for _, sentinel := range []error{
		ErrVendorIDInvalid, ErrRequisitionNotApproved, ErrItemsEmpty,
		ErrDescriptionInvalid, ErrQtyInvalid, ErrUnitPriceInvalid, ErrCurrencyInvalid,
		ErrIllegalTransition, ErrHasReceipts, ErrNotFound, ErrIDInvalid,
		requisition.ErrNotFound, requisition.ErrIDInvalid, requisition.ErrIllegalTransition,
		vendor.ErrNotFound, vendor.ErrDeactivated,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("purchaseorder: %s: %w", what, err)
}

// RFC3339 renders a timestamp for API responses.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
