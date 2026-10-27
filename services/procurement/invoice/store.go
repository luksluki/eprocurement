// Store persists vendor invoices, always scoped to one tenant through the
// RLS helpers. Create runs in one transaction whose step order is pinned
// below; every step either commits with the invoice and its lines — or
// nothing lands. The PO is locked but never mutated (#39: no qty_invoiced
// column, no status move), and no outbox row is written (the §14.6 catalog
// names invoice.matched/invoice.discrepancy only — #40's events; the
// vendor/requisition creation precedent).
package invoice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
)

// Store persists invoices, always scoped to one tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// IO failure codes; the gRPC API and the gateway carry them through to
// clients unchanged (same contract as the domain's validation sentinels —
// the vendor/goods-receipt stores' split).
var (
	ErrIDInvalid       = errors.New("inv_id_invalid")
	ErrNotFound        = errors.New("inv_not_found")
	ErrNumberDuplicate = errors.New("inv_number_duplicate")
)

const invoiceColumns = `id, tenant_id, po_id, vendor_id, number, currency,
		       total_cents, status, coalesce(match_result::text, ''), created_at, created_by`

// CreateInput is one invoice to record against a PO: the PO id, the vendor's
// invoice number, the lines, and the acting user.
type CreateInput struct {
	POID   string
	Number string
	Items  []Item
	Actor  string
}

// Create records one vendor invoice (#39) in a single tenant transaction
// with a pinned step order:
//
//  1. validate the request shape (ValidateNumber, ValidateItems — non-empty
//     trimmed number within the bound; non-empty lines, qty > 0,
//     unit_price >= 0, unique line ids) before the transaction opens;
//  2. lock the PO row FOR UPDATE (purchaseorder.LoadForUpdate — the same
//     serialization point as every #36 lifecycle transition and #37 receipt,
//     so a concurrent issue/close/receipt/invoice pair serializes here) —
//     vendor_id and currency are derived from this locked aggregate, never
//     from client input, and the PO's columns are never written (#39);
//  3. line membership: every po_item_id must name a line of this PO
//     (inv_po_item_unknown);
//  4. recompute total_cents server-side from the lines, then the cumulative
//     over-billing guard: SUM(total_cents) of the PO's existing invoices +
//     this invoice <= po.total_cents, strictly, no tolerance (tolerance is
//     the #40 match's concern, FR-08) — a violation answers the typed
//     *OverBillError whose Error() is the pinned detail line;
//  5. status gate: only acknowledged/partially_received/received POs may be
//     invoiced (maintainer decision 2026-10-27, the GRN #37 set) — anything
//     else is purchaseorder.ErrIllegalTransition. The cumulative guard runs
//     before the status edge (the #37 pinned order: a billing that cannot
//     fit answers inv_over_bill even on a closed PO, not the transition
//     code);
//  6. insert the invoice header and its lines; a duplicate
//     (tenant_id, vendor_id, number) fails the UNIQUE index →
//     ErrNumberDuplicate (the same number under a different vendor is a
//     different invoice and passes);
//  7. backstop re-SUM: total the PO's invoices again inside this
//     transaction, now including the new row, and reject with the same
//     typed error if the sum exceeds po.total_cents — unreachable under the
//     lock + step 4, but the belt-and-braces twin of #37's guarded UPDATE.
//
// No outbox row: creation is a plain insert (the vendor/requisition creation
// precedent); the §14.6 catalog's invoice.* events belong to the #40 match.
// Any failure rolls the whole transaction back: no invoice rows, no lines,
// no PO change (there never is one). Cross-tenant PO ids are
// indistinguishable from missing ones (po_not_found via RLS); non-UUID ids
// fail fast before the transaction opens.
func (s *Store) Create(ctx context.Context, tenantID string, in CreateInput) (Invoice, error) {
	in.Number = strings.TrimSpace(in.Number)
	if err := ValidateNumber(in.Number); err != nil {
		return Invoice{}, err
	}
	if err := ValidateItems(in.Items); err != nil {
		return Invoice{}, err
	}
	if _, err := uuid.Parse(in.POID); err != nil {
		return Invoice{}, purchaseorder.ErrIDInvalid
	}
	var inv Invoice
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Step 2: lock the PO — the shared serialization point, and the
		// source of vendor_id/currency.
		po, err := purchaseorder.LoadForUpdate(ctx, tx, in.POID)
		if err != nil {
			return err
		}
		// Step 3: line membership against the locked aggregate.
		members := make(map[string]bool, len(po.Items))
		for _, it := range po.Items {
			members[it.ID] = true
		}
		for _, it := range in.Items {
			if !members[it.POItemID] {
				return ErrPOItemUnknown
			}
		}
		// Step 4: server-side total, then the cumulative over-billing guard.
		total := TotalCents(in.Items)
		var invoiced int64
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(sum(total_cents), 0) FROM procurement.invoices
			WHERE po_id = $1`, po.ID).Scan(&invoiced); err != nil {
			return err
		}
		if invoiced+total > po.TotalCents {
			return &OverBillError{
				PONumber: po.Number, Invoiced: invoiced,
				Requested: total, Total: po.TotalCents,
			}
		}
		// Step 5: the status gate, after the cumulative guard (pinned order).
		switch po.Status {
		case purchaseorder.StatusAcknowledged,
			purchaseorder.StatusPartiallyReceived,
			purchaseorder.StatusReceived:
			// invoicable
		default:
			return purchaseorder.ErrIllegalTransition
		}
		// Step 6: insert the header (vendor/currency from the locked PO) and
		// the lines.
		inv, err = insertInvoice(ctx, tx, tenantID, po, in)
		if err != nil {
			return err
		}
		// Step 7: backstop re-SUM including the new row, same typed answer.
		var sum int64
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(sum(total_cents), 0) FROM procurement.invoices
			WHERE po_id = $1`, po.ID).Scan(&sum); err != nil {
			return err
		}
		if sum > po.TotalCents {
			return &OverBillError{
				PONumber: po.Number, Invoiced: sum - total,
				Requested: total, Total: po.TotalCents,
			}
		}
		return nil
	})
	if err != nil {
		return Invoice{}, wrap("create", err)
	}
	return inv, nil
}

// Get returns one invoice (header plus lines) of tenantID; other tenants'
// invoices are indistinguishable from missing ones (RLS).
func (s *Store) Get(ctx context.Context, tenantID, id string) (Invoice, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Invoice{}, ErrIDInvalid
	}
	var inv Invoice
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		if err := scanInvoice(tx.QueryRow(ctx,
			`SELECT `+invoiceColumns+` FROM procurement.invoices WHERE id = $1`, id), &inv); err != nil {
			return err
		}
		lines, err := loadLines(ctx, tx, inv.ID)
		if err != nil {
			return err
		}
		inv.Items = lines
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, ErrNotFound
		}
		return Invoice{}, fmt.Errorf("invoice: get: %w", err)
	}
	return inv, nil
}

// insertInvoice writes the header (deriving vendor/currency from the locked
// PO) and the lines, returning the stored invoice with its row timestamps.
func insertInvoice(ctx context.Context, tx pgx.Tx, tenantID string, po purchaseorder.PurchaseOrder, in CreateInput) (Invoice, error) {
	var inv Invoice
	if err := scanInvoice(tx.QueryRow(ctx, `
		INSERT INTO procurement.invoices
			(tenant_id, po_id, vendor_id, number, currency, total_cents, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', NULLIF($7, '')::uuid)
		RETURNING `+invoiceColumns,
		tenantID, po.ID, po.VendorID, in.Number, po.Currency, TotalCents(in.Items), in.Actor), &inv); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			strings.Contains(pgErr.ConstraintName, "invoices_tenant_vendor_number_uidx") {
			return Invoice{}, ErrNumberDuplicate
		}
		return Invoice{}, err
	}
	inv.Items = append([]Item(nil), in.Items...)
	for _, it := range inv.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO procurement.invoice_lines (invoice_id, po_item_id, qty, unit_price_cents)
			VALUES ($1, $2, $3, $4)`, inv.ID, it.POItemID, it.Qty, it.UnitPriceCents); err != nil {
			return Invoice{}, err
		}
	}
	return inv, nil
}

func loadLines(ctx context.Context, tx pgx.Tx, invoiceID string) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT po_item_id, qty, unit_price_cents
		FROM procurement.invoice_lines
		WHERE invoice_id = $1
		ORDER BY created_at, id`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.POItemID, &it.Qty, &it.UnitPriceCents); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// scanInvoice reads one header row; the audit id lands as an empty string
// when NULL.
func scanInvoice(row pgx.Row, inv *Invoice) error {
	var createdBy *string
	if err := row.Scan(&inv.ID, &inv.TenantID, &inv.POID, &inv.VendorID,
		&inv.Number, &inv.Currency, &inv.TotalCents, &inv.Status,
		&inv.MatchResult, &inv.CreatedAt, &createdBy); err != nil {
		return err
	}
	if createdBy != nil {
		inv.CreatedBy = *createdBy
	}
	return nil
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels (this package's shape/IO codes, the typed over-billing error,
// and the purchase-order aggregate's guards this store crosses) through
// bare, so err.Error() carries the code — or the pinned over-billing detail
// line — verbatim through gRPC.
func wrap(what string, err error) error {
	var over *OverBillError
	if errors.As(err, &over) {
		return err
	}
	for _, sentinel := range []error{
		ErrNumberInvalid, ErrItemsEmpty, ErrQtyInvalid, ErrUnitPriceInvalid,
		ErrPOItemDuplicate, ErrPOItemUnknown, ErrOverBill,
		ErrIDInvalid, ErrNotFound, ErrNumberDuplicate,
		purchaseorder.ErrIllegalTransition, purchaseorder.ErrNotFound,
		purchaseorder.ErrIDInvalid,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("invoice: %s: %w", what, err)
}

// RFC3339 renders a timestamp for API responses.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
