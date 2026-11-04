// Store persists the three-way match (#40): the per-tenant tolerance
// settings (procurement.match_settings) and the match transaction itself.
// Match runs in one transaction whose step order is pinned below; every
// step either commits with the invoice's new status, its match_result, and
// exactly one invoice.matched / invoice.discrepancy outbox row — or nothing
// lands. The engine (match.go) is pure; this store owns every read and
// write, always scoped to one tenant through the RLS helpers.
package match

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
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/outbox"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
)

// Store persists match settings and match results, always scoped to one
// tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Outbox event types every successful match writes (PRD §14.6 catalog
// entries; two segments, so the §14.5 invoice.* topic bindings deliver
// them). Exactly one of them commits per run — matched or discrepancy.
const (
	OutboxMatched     = "invoice.matched"
	OutboxDiscrepancy = "invoice.discrepancy"
)

// EventPayload is the JSONB payload of both match outbox events. It carries
// the verdict's minimum: the invoice's identity, the PO's number, the
// landed status, and the per-line result — the same ResultLine shape
// invoices.match_result stores, so consumers read one format. The
// envelope's entity_type/entity_id name the invoice (§14.6: both events are
// invoice.* on entity_type=invoice), keeping the invoice timeline whole.
type EventPayload struct {
	InvoiceID string       `json:"invoice_id"`
	Number    string       `json:"number"`
	PONumber  string       `json:"po_number"`
	Status    string       `json:"status"`
	Lines     []ResultLine `json:"lines"`
	MatchedBy string       `json:"matched_by"`
}

// Match applies the three-way match to one invoice of tenantID in a single
// tenant transaction with a pinned step order:
//
//  1. fail fast on a non-UUID id before the transaction opens
//     (invoice.ErrIDInvalid);
//  2. resolve the invoice header unlocked — ids are immutable, this only
//     finds the PO to lock (RLS: cross-tenant ids are indistinguishable
//     from missing ones, invoice.ErrNotFound);
//  3. lock the PO row FOR UPDATE (purchaseorder.LoadForUpdate — the same
//     serialization point as every lifecycle transition, receipt, and
//     invoice create, so concurrent matches — of this invoice or a sibling
//     invoice of the same PO — serialize here, and the cumulative invoiced
//     sum reads one consistent snapshot);
//  4. re-read the invoice header FOR UPDATE with its lines, then the
//     re-run guard: status matched rejects the run
//     (invoice.ErrAlreadyMatched, 409); pending and discrepancy may be
//     (re-)matched (maintainer decision 2026-10-28 — a late GRN resolving
//     a discrepancy re-runs through here, appending its own event);
//  5. read the tenant's tolerances from procurement.match_settings — no
//     row means the effective defaults 0%/0% (strict);
//  6. sum the PO's cumulative invoiced qty per line over ALL its invoices,
//     this one included (the maintainer's cumulative basis: two invoices
//     that each fit but together exceed the receipts are caught);
//  7. engine.Run — the pure, integer-only decision;
//  8. guarded UPDATE of status + match_result (WHERE status IN
//     ('pending','discrepancy')) — RowsAffected 0 answers
//     invoice.ErrAlreadyMatched, the belt-and-braces twin of the lock for
//     a race the lock already prevents;
//  9. append exactly one outbox row — invoice.matched or
//     invoice.discrepancy — committed with everything above (the
//     grn.posted pattern; ADR-0001 — no dual writes).
//
// Any failure rolls the whole transaction back: no status move, no
// match_result, no outbox row. The purchase_orders row is never written.
func (s *Store) Match(ctx context.Context, tenantID, invoiceID, actor string) (invoice.Invoice, Result, error) {
	if _, err := uuid.Parse(invoiceID); err != nil {
		return invoice.Invoice{}, Result{}, invoice.ErrIDInvalid
	}
	var inv invoice.Invoice
	var result Result
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Step 2: resolve the invoice's PO — unlocked; ids never change.
		var poID string
		if err := tx.QueryRow(ctx, `
			SELECT po_id FROM procurement.invoices WHERE id = $1`, invoiceID).Scan(&poID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return invoice.ErrNotFound
			}
			return err
		}
		// Step 3: lock the PO — the shared serialization point.
		po, err := purchaseorder.LoadForUpdate(ctx, tx, poID)
		if err != nil {
			return err
		}
		// Step 4: the locked header re-read plus lines, then the re-run
		// guard on this post-lock view.
		inv, err = loadInvoiceForUpdate(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if err := invoice.MatchGuard(inv.Status); err != nil {
			return err
		}
		// Step 5: the tenant's tolerances; absence is the strict default.
		settings, err := loadSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		// Step 6: cumulative invoiced qty per PO line, this invoice
		// included.
		invoiced, err := loadInvoicedQty(ctx, tx, po.ID)
		if err != nil {
			return err
		}
		// Step 7: the pure decision.
		lines := make([]Line, 0, len(inv.Items))
		for _, it := range inv.Items {
			lines = append(lines, Line{
				POItemID:       it.POItemID,
				Qty:            it.Qty,
				UnitPriceCents: it.UnitPriceCents,
				QtyInvoicedCum: invoiced[it.POItemID],
			})
		}
		poItems := make([]POItem, 0, len(po.Items))
		for _, it := range po.Items {
			poItems = append(poItems, POItem{
				ID: it.ID, QtyOrdered: it.Qty, QtyReceived: it.QtyReceived,
				UnitPriceCents: it.UnitPriceCents,
			})
		}
		result, err = Run(Input{POItems: poItems, Lines: lines, Settings: settings})
		if err != nil {
			return err
		}
		// Step 8: the guarded status + match_result write.
		status := invoice.StatusDiscrepancy
		if result.Matched {
			status = invoice.StatusMatched
		}
		tag, err := tx.Exec(ctx, `
			UPDATE procurement.invoices
			SET status = $2, match_result = $3::jsonb
			WHERE id = $1 AND status IN ('pending', 'discrepancy')`,
			inv.ID, status, result.ResultJSON())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invoice.ErrAlreadyMatched // lost the race despite the lock
		}
		inv.Status = status
		inv.MatchResult = result.ResultJSON()
		// Step 9: exactly one outbox row — matched or discrepancy.
		eventType := OutboxDiscrepancy
		if result.Matched {
			eventType = OutboxMatched
		}
		return outbox.AppendTx(ctx, tx, outbox.Event{
			ID: uuid.NewString(), TenantID: tenantID, EventType: eventType,
			Version: 1,
			Payload: EventPayload{
				InvoiceID: inv.ID, Number: inv.Number, PONumber: po.Number,
				Status: status, Lines: result.Lines, MatchedBy: actor,
			},
			ActorID:    actor,
			ActorIP:    logger.ActorIP(ctx),
			TraceID:    logger.TraceID(ctx),
			EntityType: "invoice",
			EntityID:   inv.ID,
			OccurredAt: time.Now().UTC(),
		})
	})
	if err != nil {
		return invoice.Invoice{}, Result{}, wrap("match", err)
	}
	return inv, result, nil
}

// GetSettings returns the tenant's tolerances. A tenant with no row gets
// the effective defaults 0%/0% — never an error: strict is the shipped
// behavior, not a misconfiguration.
func (s *Store) GetSettings(ctx context.Context, tenantID string) (Settings, error) {
	var out Settings
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = loadSettingsTx(ctx, tx)
		return err
	})
	if err != nil {
		return Settings{}, wrap("get settings", err)
	}
	return out, nil
}

// ReplaceSettings validates then upserts the tenant's tolerances in one
// transaction. The new values apply to every match run that starts after
// the commit; already-stored match results are immutable history.
func (s *Store) ReplaceSettings(ctx context.Context, tenantID string, in Settings, actor string) (Settings, error) {
	if err := ValidateSettings(in); err != nil {
		return Settings{}, err
	}
	out := in
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO procurement.match_settings (tenant_id, qty_pct, price_pct, updated_by)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
			ON CONFLICT (tenant_id) DO UPDATE
			SET qty_pct = EXCLUDED.qty_pct,
			    price_pct = EXCLUDED.price_pct,
			    updated_at = now(),
			    updated_by = EXCLUDED.updated_by
			RETURNING qty_pct, price_pct`,
			tenantID, in.QtyPct, in.PricePct, actor).Scan(&out.QtyPct, &out.PricePct)
	})
	if err != nil {
		return Settings{}, wrap("replace settings", err)
	}
	return out, nil
}

// loadSettingsTx reads the tenant's tolerance row inside tx; no row is the
// strict default, not an error.
func loadSettingsTx(ctx context.Context, tx pgx.Tx) (Settings, error) {
	s := DefaultSettings()
	err := tx.QueryRow(ctx,
		`SELECT qty_pct, price_pct FROM procurement.match_settings`).Scan(&s.QtyPct, &s.PricePct)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	return s, nil
}

// loadInvoicedQty sums qty over every line of every invoice of one PO,
// grouped by po_item_id. It runs under the PO row lock inside the match
// transaction, so the sums and this invoice's own rows are one snapshot.
func loadInvoicedQty(ctx context.Context, tx pgx.Tx, poID string) (map[string]int32, error) {
	rows, err := tx.Query(ctx, `
		SELECT il.po_item_id, sum(il.qty)
		FROM procurement.invoice_lines il
		JOIN procurement.invoices i ON i.id = il.invoice_id
		WHERE i.po_id = $1
		GROUP BY il.po_item_id`, poID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int32{}
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		out[id] = int32(sum)
	}
	return out, rows.Err()
}

// loadInvoiceForUpdate reads one invoice header under the row lock plus its
// lines — the post-lock view the re-run guard and the engine input build
// on. RLS has already scoped the read to the tenant.
func loadInvoiceForUpdate(ctx context.Context, tx pgx.Tx, id string) (invoice.Invoice, error) {
	var inv invoice.Invoice
	var createdBy *string
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, po_id, vendor_id, number, currency,
		       total_cents, status, coalesce(match_result::text, ''),
		       created_at, created_by
		FROM procurement.invoices WHERE id = $1 FOR UPDATE`, id).
		Scan(&inv.ID, &inv.TenantID, &inv.POID, &inv.VendorID,
			&inv.Number, &inv.Currency, &inv.TotalCents, &inv.Status,
			&inv.MatchResult, &inv.CreatedAt, &createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return invoice.Invoice{}, invoice.ErrNotFound
	}
	if err != nil {
		return invoice.Invoice{}, err
	}
	if createdBy != nil {
		inv.CreatedBy = *createdBy
	}
	rows, err := tx.Query(ctx, `
		SELECT po_item_id, qty, unit_price_cents
		FROM procurement.invoice_lines
		WHERE invoice_id = $1
		ORDER BY created_at, id`, id)
	if err != nil {
		return invoice.Invoice{}, err
	}
	defer rows.Close()
	inv.Items = []invoice.Item{}
	for rows.Next() {
		var it invoice.Item
		if err := rows.Scan(&it.POItemID, &it.Qty, &it.UnitPriceCents); err != nil {
			return invoice.Invoice{}, err
		}
		inv.Items = append(inv.Items, it)
	}
	return inv, rows.Err()
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels through bare (this package's settings codes, the invoice
// aggregate's guards this store crosses, and the purchase-order lock's IO
// codes), so err.Error() carries the code verbatim through gRPC.
func wrap(what string, err error) error {
	for _, sentinel := range []error{
		ErrQtyPctInvalid, ErrPricePctInvalid, ErrPOItemUnknown,
		invoice.ErrAlreadyMatched, invoice.ErrNotFound, invoice.ErrIDInvalid,
		invoice.ErrPOItemUnknown, invoice.ErrItemsEmpty,
		purchaseorder.ErrNotFound, purchaseorder.ErrIDInvalid,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("match: %s: %w", what, err)
}
