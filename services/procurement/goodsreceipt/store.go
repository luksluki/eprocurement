// Store persists goods receipts, always scoped to one tenant through the
// RLS helpers. Post runs in one transaction whose step order is pinned
// below; every step either commits with the GRN, its items, the advanced
// po_items.qty_received, the (optional) PO status move, and exactly one
// grn.posted outbox row — or nothing lands.
package goodsreceipt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/outbox"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
)

// Store persists goods receipts, always scoped to one tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// OutboxPosted is the outbox event every successful goods receipt writes:
// "grn.posted" — a PRD §14.6 catalog entry (two segments, so the §14.5
// grn.* topic bindings deliver it). Pinned with its payload in
// docs/schema/procurement.md.
const OutboxPosted = "grn.posted"

// PostedPayload is the JSONB payload of the grn.posted outbox event. The
// fields are pinned (docs/schema/procurement.md): the GRN's identity, the
// PO's number and post-receipt status, the receipt's lines, and who
// received. The envelope's entity_type/entity_id name the purchase order
// (the pr.step_approved precedent, #31) so the PO timeline stays whole.
type PostedPayload struct {
	GRNID      string        `json:"grn_id"`
	Number     string        `json:"number"`
	PONumber   string        `json:"po_number"`
	POStatus   string        `json:"po_status"`
	Items      []PayloadItem `json:"items"`
	ReceivedBy string        `json:"received_by"`
}

// PayloadItem is one receipt line in the grn.posted payload.
type PayloadItem struct {
	POItemID    string `json:"po_item_id"`
	QtyReceived int32  `json:"qty_received"`
}

// PostInput is one goods receipt to post against a PO: the PO id, the
// receipt's lines, free-form notes, and the acting user.
type PostInput struct {
	POID  string
	Items []Item
	Notes string
	Actor string
}

// Post records one goods receipt (#37, FR-07) in a single tenant
// transaction with a pinned step order:
//
//  1. validate the request shape (Validate — non-empty lines, qty > 0,
//     unique line ids, notes bound) before the transaction opens;
//  2. lock the PO row FOR UPDATE (purchaseorder.LoadForUpdate — the same
//     serialization point as every #36 lifecycle transition, so a concurrent
//     issue/close/receipt pair serializes here: exactly one lands);
//  3. purchaseorder.ApplyReceipt — line membership, the cumulative
//     over-receipt guard (first violating line in request order), the
//     status edge, the qty_received advances, and the target status via the
//     three pinned branches (first partial → partially_received, full →
//     received, further partial → same status, no transition at all);
//  4. allocate the next GRN-%06d number from grn_counters (UPDATE …
//     RETURNING under the row lock) after the guards, so a rejected receipt
//     rolls the increment back and burns no number;
//  5. insert the goods receipt and its lines;
//  6. advance po_items.qty_received with a WHERE qty_received + $2 <=
//     qty_ordered backstop under the lock;
//  7. move purchase_orders.status only when the aggregate status changed
//     (branches a/b) with the from-status re-check belt and braces — the
//     same-status branch writes no purchase_orders columns at all;
//  8. append exactly one grn.posted outbox row — every branch, including
//     the same-status one — committed with everything above.
//
// Any failure rolls the whole transaction back: no GRN rows, no qty move,
// no status move, no outbox row, no burned number. Cross-tenant PO ids are
// indistinguishable from missing ones (po_not_found via RLS); non-UUID ids
// fail fast before the transaction opens.
func (s *Store) Post(ctx context.Context, tenantID string, in PostInput) (GoodsReceipt, purchaseorder.PurchaseOrder, error) {
	if err := Validate(in.Items, in.Notes); err != nil {
		return GoodsReceipt{}, purchaseorder.PurchaseOrder{}, err
	}
	if _, err := uuid.Parse(in.POID); err != nil {
		return GoodsReceipt{}, purchaseorder.PurchaseOrder{}, purchaseorder.ErrIDInvalid
	}
	var grn GoodsReceipt
	var po purchaseorder.PurchaseOrder
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Step 2: lock the PO — the shared serialization point.
		loaded, err := purchaseorder.LoadForUpdate(ctx, tx, in.POID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		// Step 3: the domain owns the guards, the qty advances, and the
		// status branch.
		from := loaded.Status
		lines := make([]purchaseorder.ReceiptLine, 0, len(in.Items))
		for _, it := range in.Items {
			lines = append(lines, purchaseorder.ReceiptLine{POItemID: it.POItemID, Qty: it.QtyReceived})
		}
		if err := loaded.ApplyReceipt(now, in.Actor, lines); err != nil {
			return err
		}
		// Step 4: number allocation — inside the transaction, after the
		// guards, so a rejected receipt burns no number.
		number, err := nextNumber(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		grn = GoodsReceipt{
			ID: uuid.NewString(), TenantID: tenantID, POID: loaded.ID,
			Number: number, Notes: in.Notes, ReceivedBy: in.Actor,
			ReceivedAt: now, CreatedAt: now,
			Items: append([]Item(nil), in.Items...),
		}
		// Step 5: insert the GRN and its lines.
		if err := insertGRN(ctx, tx, &grn); err != nil {
			return err
		}
		// Step 6: the qty advances, with the DB backstop under the lock.
		for _, it := range in.Items {
			tag, err := tx.Exec(ctx, `
				UPDATE procurement.po_items
				SET qty_received = qty_received + $2
				WHERE id = $1 AND qty_received + $2 <= qty_ordered`,
				it.POItemID, it.QtyReceived)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// Unreachable under the lock + the domain guard; render the
				// pinned detail anyway so even this path answers exactly.
				var received, ordered int32
				if err := tx.QueryRow(ctx, `
					SELECT qty_received, qty_ordered FROM procurement.po_items
					WHERE id = $1`, it.POItemID).Scan(&received, &ordered); err != nil {
					return err
				}
				return &purchaseorder.OverReceiptError{
					POItemID: it.POItemID, Received: received,
					Requested: it.QtyReceived, Ordered: ordered,
				}
			}
		}
		// Step 7: the status move — only when ApplyReceipt changed it.
		if loaded.Status != from {
			tag, err := tx.Exec(ctx, `
				UPDATE procurement.purchase_orders
				SET status = $2, issued_at = $3, updated_at = now(), updated_by = NULLIF($4, '')::uuid
				WHERE id = $1 AND status = $5`,
				loaded.ID, loaded.Status, loaded.IssuedAt, in.Actor, from)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return purchaseorder.ErrIllegalTransition // lost the race despite the lock
			}
		}
		// Step 8: exactly one grn.posted outbox row — every branch.
		items := make([]PayloadItem, 0, len(in.Items))
		for _, it := range in.Items {
			items = append(items, PayloadItem{POItemID: it.POItemID, QtyReceived: it.QtyReceived})
		}
		if err := outbox.AppendTx(ctx, tx, outbox.Event{
			ID: uuid.NewString(), TenantID: tenantID, EventType: OutboxPosted,
			Version: 1,
			Payload: PostedPayload{
				GRNID: grn.ID, Number: grn.Number, PONumber: loaded.Number,
				POStatus: loaded.Status, Items: items, ReceivedBy: in.Actor,
			},
			ActorID:    in.Actor,
			ActorIP:    logger.ActorIP(ctx),
			TraceID:    logger.TraceID(ctx),
			EntityType: "purchase_order",
			EntityID:   loaded.ID,
			OccurredAt: now,
		}); err != nil {
			return err
		}
		po = loaded
		return nil
	})
	if err != nil {
		return GoodsReceipt{}, purchaseorder.PurchaseOrder{}, wrap("post", err)
	}
	return grn, po, nil
}

// nextNumber allocates the next GRN-%06d number for tenantID inside tx, the
// same counter pattern as the PR (0004) and PO (0007) numbers: the row is
// initialized lazily from the tenant's highest existing number (INSERT … ON
// CONFLICT DO NOTHING), then UPDATE … RETURNING takes the row lock that
// serializes all allocators of the tenant. The increment rolls back with a
// rejected receipt, so the per-tenant sequence is gap-free.
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO procurement.grn_counters (tenant_id, last_number)
		SELECT $1, coalesce(max((substring(number FROM 5))::bigint), 0)
		FROM procurement.goods_receipts
		WHERE tenant_id = $1 AND number ~ '^GRN-[0-9]+$'
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID); err != nil {
		return "", fmt.Errorf("counter init: %w", err)
	}
	var n int64
	if err := tx.QueryRow(ctx, `
		UPDATE procurement.grn_counters
		SET last_number = last_number + 1
		WHERE tenant_id = $1
		RETURNING last_number`, tenantID).Scan(&n); err != nil {
		return "", fmt.Errorf("counter next: %w", err)
	}
	return fmt.Sprintf("GRN-%06d", n), nil
}

func insertGRN(ctx context.Context, tx pgx.Tx, grn *GoodsReceipt) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO procurement.goods_receipts
			(id, tenant_id, po_id, number, received_by, received_at, notes)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7)`,
		grn.ID, grn.TenantID, grn.POID, grn.Number, grn.ReceivedBy,
		grn.ReceivedAt, grn.Notes); err != nil {
		return err
	}
	for _, it := range grn.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO procurement.goods_receipt_items (grn_id, po_item_id, qty_received)
			VALUES ($1, $2, $3)`, grn.ID, it.POItemID, it.QtyReceived); err != nil {
			return err
		}
	}
	return nil
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels (this package's shape codes, the purchase-order aggregate's
// receipt guards and IO codes) through bare, so err.Error() carries the
// code — or the pinned over-receipt detail line — verbatim through gRPC.
func wrap(what string, err error) error {
	var over *purchaseorder.OverReceiptError
	if errors.As(err, &over) {
		return err
	}
	for _, sentinel := range []error{
		ErrItemsEmpty, ErrQtyInvalid, ErrPOItemDuplicate, ErrNotesInvalid,
		purchaseorder.ErrOverReceipt, purchaseorder.ErrReceiptItemUnknown,
		purchaseorder.ErrReceiptItemDuplicate, purchaseorder.ErrIllegalTransition,
		purchaseorder.ErrNotFound, purchaseorder.ErrIDInvalid,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("goodsreceipt: %s: %w", what, err)
}

// RFC3339 renders a timestamp for API responses.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
