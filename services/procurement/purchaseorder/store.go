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
		SELECT description, qty_ordered, qty_received, unit_price_cents
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
		if err := rows.Scan(&it.Description, &it.Qty, &it.QtyReceived, &it.UnitPriceCents); err != nil {
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
// sentinels (domain validation, IO, and the requisition/vendor gates this
// flow crosses) through bare, so err.Error() carries the code verbatim
// through gRPC — same contract as the requisition/vendor stores.
func wrap(what string, err error) error {
	for _, sentinel := range []error{
		ErrVendorIDInvalid, ErrRequisitionNotApproved, ErrItemsEmpty,
		ErrDescriptionInvalid, ErrQtyInvalid, ErrUnitPriceInvalid, ErrCurrencyInvalid,
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
