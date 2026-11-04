package invoice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	invTenantA   = "88888888-0000-0000-0000-000000000001"
	invTenantB   = "88888888-0000-0000-0000-000000000002"
	invRequester = "88888888-0000-0000-0000-0000000000aa"
	invApprover  = "88888888-0000-0000-0000-0000000000bb"
	invBuyer     = "88888888-0000-0000-0000-0000000000cc"
	invFinance   = "88888888-0000-0000-0000-0000000000dd"
)

// openTestDB mirrors the goodsreceipt harness: apply migrations, open a
// pool, skip without TEST_DATABASE_URL.
func openTestDB(t *testing.T) *pgxpool.Pool {
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
	return db
}

// cleanupINV wipes everything the invoice tests create in the shared test
// tenants: outbox rows, invoices (lines cascade), goods receipts (items
// cascade), GRN counters, purchase orders (po_items cascade), PO counters,
// requisitions (items/steps cascade), PR counters, approval rules, and the
// test vendors.
func cleanupINV(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		ctx := context.Background()
		for _, stmt := range []string{
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.invoices WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.goods_receipts WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.grn_counters WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.purchase_orders WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.po_counters WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.requisitions WHERE tenant_id IN ($1, $2) AND number LIKE 'PR-%'`,
			`DELETE FROM procurement.requisition_counters WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.approval_rules WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'INV-39-%'`,
		} {
			if _, err := tx.Exec(ctx, stmt, invTenantA, invTenantB); err != nil {
				return err
			}
		}
		return nil
	})
}

// invoicablePO drives the real flow — rules, requisition, submit, approve,
// convert, issue, acknowledge — so every invoice lands on a PO the domain
// state machine actually walked here. vendorCode picks the fixture vendor
// (two vendors prove the same number under different vendors is accepted).
func invoicablePO(t *testing.T, db *pgxpool.Pool, tenantID, vendorCode string, basket []requisition.Item) purchaseorder.PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	if _, err := approval.NewStore(db).ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	v, err := vendor.NewStore(db).Create(ctx, tenantID, vendor.CreateInput{
		Code: vendorCode, Name: "PT Invoice Fixture", UserID: invBuyer,
	})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: invRequester, Currency: "IDR", Items: basket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, invRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
		_, err := apps.MaterializeTx(ctx, tx, req)
		return err
	}); err != nil {
		t.Fatalf("submit requisition: %v", err)
	}
	steps, err := apps.ListSteps(ctx, tenantID, r.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("list steps = (%+v, %v), want one", steps, err)
	}
	if _, _, err := apps.Decide(ctx, tenantID, approval.DecideInput{
		StepID: steps[0].ID, Decision: approval.DecisionApprove,
		ActorID: invApprover, Roles: []string{"approver"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	pos := purchaseorder.NewStore(db)
	po, _, err := pos.CreateFromRequisition(ctx, tenantID, r.ID, v.ID, invBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, err := pos.Issue(ctx, tenantID, po.ID, invBuyer); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ack, err := pos.Acknowledge(ctx, tenantID, po.ID, invBuyer)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	return ack
}

var invBasket = []requisition.Item{
	{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"}, // line 15,000,000
	{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},   // line 450,000
}

// invItemByQty picks the PO line with the given qty_ordered — po_items rows
// of one conversion share created_at, so load order is not the basket order
// (the dev-note 035 multiset note).
func invItemByQty(t *testing.T, po purchaseorder.PurchaseOrder, qty int32) purchaseorder.Item {
	t.Helper()
	for _, it := range po.Items {
		if it.Qty == qty {
			return it
		}
	}
	t.Fatalf("no PO line with qty_ordered %d in %+v", qty, po.Items)
	return purchaseorder.Item{}
}

// invCount counts the tenant's invoices under RLS bypass.
func invCount(t *testing.T, db *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.invoices WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return n
}

// outboxCount counts the tenant's outbox rows — invoice creation writes
// none (#39).
func outboxCount(t *testing.T, db *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.outbox WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// poAudit reads the purchase_orders audit columns the create must not move.
func poAudit(t *testing.T, db *pgxpool.Pool, poID string) (status, updatedAt, updatedBy string) {
	t.Helper()
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT status, updated_at::text, coalesce(updated_by::text, '')
			FROM procurement.purchase_orders WHERE id = $1`, poID).
			Scan(&status, &updatedAt, &updatedBy)
	}); err != nil {
		t.Fatalf("read po audit: %v", err)
	}
	return
}

// invMatchResult reads the stored match_result of one invoice (NULL lands
// as "<nil>").
func invMatchResult(t *testing.T, db *pgxpool.Pool, invoiceID string) any {
	t.Helper()
	var result any
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT match_result FROM procurement.invoices WHERE id = $1`, invoiceID).Scan(&result)
	}); err != nil {
		t.Fatalf("read match_result: %v", err)
	}
	return result
}

// AC (#39 happy path): creating an invoice derives vendor/currency from the
// locked PO, recomputes the total server-side, lands status pending with
// match_result NULL — and writes no outbox row and no purchase_orders
// change at all. Invoicing stays legal as receipts move the PO through
// partially_received and received (the #37 status set), and the cumulative
// guard accepts the exact fit (invoiced + requested == total).
func TestStoreCreateInvoiceHappyPath(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	laptop, mouse := invItemByQty(t, po, 2), invItemByQty(t, po, 3)

	beforeStatus, beforeUpdated, beforeBy := poAudit(t, db, po.ID)
	outboxBase := outboxCount(t, db, invTenantA) // the harness flow's own rows
	inv, err := s.Create(ctx, invTenantA, CreateInput{
		POID: po.ID, Number: "  INV/2026/0001  ",
		Items: []Item{{POItemID: laptop.ID, Qty: 2, UnitPriceCents: 7500000}},
		Actor: invFinance,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.Number != "INV/2026/0001" {
		t.Fatalf("number = %q, want trimmed INV/2026/0001", inv.Number)
	}
	if inv.VendorID != po.VendorID || inv.Currency != po.Currency || inv.POID != po.ID {
		t.Fatalf("derived fields = (%s, %s, %s), want from PO (%s, %s, %s)",
			inv.VendorID, inv.Currency, inv.POID, po.VendorID, po.Currency, po.ID)
	}
	if inv.TotalCents != 15000000 || inv.Status != StatusPending || inv.MatchResult != "" {
		t.Fatalf("invoice = (%d total, %s, %q)", inv.TotalCents, inv.Status, inv.MatchResult)
	}
	if inv.CreatedBy != invFinance || inv.CreatedAt.IsZero() {
		t.Fatalf("audit fields = (%s, %v)", inv.CreatedBy, inv.CreatedAt)
	}
	if result := invMatchResult(t, db, inv.ID); result != nil {
		t.Fatalf("stored match_result = %v, want NULL", result)
	}
	// No outbox row of its own, no purchase_orders change (#39 provenance
	// decisions): the count is exactly the harness flow's.
	if n := outboxCount(t, db, invTenantA); n != outboxBase {
		t.Fatalf("outbox rows after create = %d, want the pre-create %d", n, outboxBase)
	}
	afterStatus, afterUpdated, afterBy := poAudit(t, db, po.ID)
	if afterStatus != beforeStatus || afterUpdated != beforeUpdated || afterBy != beforeBy {
		t.Fatalf("create moved purchase_orders: (%s,%s,%s) -> (%s,%s,%s)",
			beforeStatus, beforeUpdated, beforeBy, afterStatus, afterUpdated, afterBy)
	}

	// Get returns the header with the lines.
	got, err := s.Get(ctx, invTenantA, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].POItemID != laptop.ID ||
		got.Items[0].Qty != 2 || got.Items[0].UnitPriceCents != 7500000 {
		t.Fatalf("got items = %+v", got.Items)
	}

	// A partial receipt moves the PO to partially_received — invoicing stays
	// legal; the exact fit (invoiced + requested == total) passes.
	if _, _, err := goodsreceipt.NewStore(db).Post(ctx, invTenantA, goodsreceipt.PostInput{
		POID: po.ID, Items: []goodsreceipt.Item{{POItemID: laptop.ID, QtyReceived: 1}}, Actor: invBuyer,
	}); err != nil {
		t.Fatalf("post partial grn: %v", err)
	}
	inv2, err := s.Create(ctx, invTenantA, CreateInput{
		POID: po.ID, Number: "INV/2026/0002",
		Items: []Item{{POItemID: mouse.ID, Qty: 3, UnitPriceCents: 150000}},
		Actor: invFinance,
	})
	if err != nil {
		t.Fatalf("create on partially_received: %v", err)
	}
	if inv2.TotalCents != 450000 || 15000000+450000 != po.TotalCents {
		t.Fatalf("exact fit rejected or mis-totaled: inv2 = %d, po = %d", inv2.TotalCents, po.TotalCents)
	}
	if status, _, _ := poAudit(t, db, po.ID); status != purchaseorder.StatusPartiallyReceived {
		t.Fatalf("po status = %s, want partially_received", status)
	}
	// The grn.posted row is the receipt's, not the invoice's: exactly one
	// new outbox row since the pre-create baseline, written by the GRN post.
	if n := outboxCount(t, db, invTenantA); n != outboxBase+1 {
		t.Fatalf("outbox rows = %d, want baseline+1 (grn.posted only)", n)
	}

	// Complete the receipt (received) — the PO stays invoicable in status,
	// but the budget is exhausted: anything more answers the typed
	// over-bill error, not a status rejection.
	if _, _, err := goodsreceipt.NewStore(db).Post(ctx, invTenantA, goodsreceipt.PostInput{
		POID: po.ID,
		Items: []goodsreceipt.Item{
			{POItemID: laptop.ID, QtyReceived: 1},
			{POItemID: mouse.ID, QtyReceived: 3},
		},
		Actor: invBuyer,
	}); err != nil {
		t.Fatalf("post completing grn: %v", err)
	}
	if status, _, _ := poAudit(t, db, po.ID); status != purchaseorder.StatusReceived {
		t.Fatalf("po status = %s, want received", status)
	}
	_, err = s.Create(ctx, invTenantA, CreateInput{
		POID: po.ID, Number: "INV/2026/0003",
		Items: []Item{{POItemID: mouse.ID, Qty: 1, UnitPriceCents: 150000}},
		Actor: invFinance,
	})
	wantOver := fmt.Sprintf("inv_over_bill: po_number=%s invoiced=15450000 requested=150000 total=15450000", po.Number)
	if err == nil || err.Error() != wantOver {
		t.Fatalf("post-budget err = %v, want %q", err, wantOver)
	}
	if n := invCount(t, db, invTenantA); n != 2 {
		t.Fatalf("invoices at end = %d, want 2", n)
	}
	// Only the two grn.posted rows joined since the baseline — the invoices
	// contributed none.
	if n := outboxCount(t, db, invTenantA); n != outboxBase+2 {
		t.Fatalf("outbox rows at end = %d, want baseline+2 (grn.posted only)", n)
	}
}

// AC (#39 over-billing, cumulative and strict): existing invoices plus the
// new one must fit po.total_cents with no tolerance — the violation answers
// the pinned detail line verbatim, nothing is written, and the losing racer
// of a concurrent pair gets the same typed answer (exactly one wins).
func TestStoreCreateInvoiceOverBill(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	laptop := invItemByQty(t, po, 2)

	// 9,000,000 of the 15,450,000 budget.
	if _, err := s.Create(ctx, invTenantA, CreateInput{
		POID: po.ID, Number: "INV-A-1",
		Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 9000000}},
		Actor: invFinance,
	}); err != nil {
		t.Fatalf("create first: %v", err)
	}

	// 7,000,000 more does not fit: 9,000,000 + 7,000,000 > 15,450,000.
	_, err := s.Create(ctx, invTenantA, CreateInput{
		POID: po.ID, Number: "INV-A-2",
		Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 7000000}},
		Actor: invFinance,
	})
	want := fmt.Sprintf("inv_over_bill: po_number=%s invoiced=9000000 requested=7000000 total=15450000", po.Number)
	if err == nil || err.Error() != want {
		t.Fatalf("over-bill err = %v, want %q", err, want)
	}
	if !errors.Is(err, ErrOverBill) {
		t.Fatalf("errors.Is(err, ErrOverBill) = false")
	}
	if n := invCount(t, db, invTenantA); n != 1 {
		t.Fatalf("invoices after rejection = %d, want 1", n)
	}

	// Concurrency: two parallel creates of 8,000,000 each — 9,000,000 +
	// 8,000,000 already exceeds, but from a clean budget two of 8,000,000
	// cannot both fit; exactly one wins. Fresh PO for a clean slate.
	po2 := invoicablePO(t, db, invTenantA, "INV-39-B", invBasket)
	laptop2 := invItemByQty(t, po2, 2)
	var wg sync.WaitGroup
	var wins, losses atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Create(ctx, invTenantA, CreateInput{
				POID: po2.ID, Number: "INV-RACE",
				Items: []Item{{POItemID: laptop2.ID, Qty: 1, UnitPriceCents: 8000000}},
				Actor: invFinance,
			})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrOverBill):
				losses.Add(1)
			default:
				t.Errorf("racer err = %v, want a win or the over-bill answer", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 1 {
		t.Fatalf("race = (%d wins, %d losses), want (1, 1)", wins.Load(), losses.Load())
	}
	var po2Invoices int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM procurement.invoices WHERE po_id = $1`, po2.ID).Scan(&po2Invoices)
	}); err != nil {
		t.Fatalf("count po2 invoices: %v", err)
	}
	if po2Invoices != 1 {
		t.Fatalf("po2 invoices after race = %d, want 1", po2Invoices)
	}
}

// AC (#39 guards, table-driven): shape and reference failures answer their
// machine codes with nothing written; only the #37 status set may be
// invoiced — and the cumulative guard fires before the status edge (a
// billing that cannot fit answers inv_over_bill even on a closed PO).
func TestStoreCreateInvoiceGuards(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	laptop := invItemByQty(t, po, 2)
	unknown := "99999999-0000-0000-0000-0000000000ee"

	create := func(in CreateInput) error {
		_, err := s.Create(ctx, invTenantA, in)
		return err
	}
	for name, tc := range map[string]struct {
		in      CreateInput
		wantErr error
		wantMsg string
	}{
		"empty number": {CreateInput{POID: po.ID,
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance}, ErrNumberInvalid, "inv_number_invalid"},
		"number over bound": {CreateInput{POID: po.ID, Number: string(make([]byte, MaxNumberLen+1)),
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance}, ErrNumberInvalid, "inv_number_invalid"},
		"empty items": {CreateInput{POID: po.ID, Number: "INV-G-1", Actor: invFinance}, ErrItemsEmpty, "inv_items_empty"},
		"zero qty": {CreateInput{POID: po.ID, Number: "INV-G-1",
			Items: []Item{{POItemID: laptop.ID, Qty: 0, UnitPriceCents: 1}}, Actor: invFinance}, ErrQtyInvalid, "inv_qty_invalid"},
		"negative price": {CreateInput{POID: po.ID, Number: "INV-G-1",
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: -1}}, Actor: invFinance}, ErrUnitPriceInvalid, "inv_unit_price_invalid"},
		"duplicate line": {CreateInput{POID: po.ID, Number: "INV-G-1",
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}, {POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}},
			Actor: invFinance}, ErrPOItemDuplicate, "inv_po_item_duplicate"},
		"unknown line": {CreateInput{POID: po.ID, Number: "INV-G-1",
			Items: []Item{{POItemID: unknown, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance}, ErrPOItemUnknown, "inv_po_item_unknown"},
		"bad po id": {CreateInput{POID: "nope", Number: "INV-G-1",
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance}, purchaseorder.ErrIDInvalid, "po_id_invalid"},
		"unknown po": {CreateInput{POID: unknown, Number: "INV-G-1",
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance}, purchaseorder.ErrNotFound, "po_not_found"},
	} {
		if err := create(tc.in); !errors.Is(err, tc.wantErr) || err.Error() != tc.wantMsg {
			t.Errorf("%s: err = (%v, %q), want (%v, %q)", name, err, errString(err), tc.wantErr, tc.wantMsg)
		}
	}

	// Draft PO: a conversion that stopped before issue cannot be invoiced.
	draft := draftPO(t, db, invTenantA)
	err := create(CreateInput{POID: draft.ID, Number: "INV-G-2",
		Items: []Item{{POItemID: draft.Items[0].ID, Qty: 1, UnitPriceCents: 400000}}, Actor: invFinance})
	if !errors.Is(err, purchaseorder.ErrIllegalTransition) || err.Error() != "po_illegal_transition" {
		t.Fatalf("draft invoice err = %v, want po_illegal_transition", err)
	}

	// Closed PO with an over-budget billing: the cumulative guard fires
	// first (the pinned order), not the status edge.
	closed := invoicablePO(t, db, invTenantA, "INV-39-C", invBasket)
	closedLaptop := invItemByQty(t, closed, 2)
	pos := purchaseorder.NewStore(db)
	grns := goodsreceipt.NewStore(db)
	if _, _, err := grns.Post(ctx, invTenantA, goodsreceipt.PostInput{
		POID: closed.ID, Items: []goodsreceipt.Item{{POItemID: closedLaptop.ID, QtyReceived: 1}}, Actor: invBuyer,
	}); err != nil {
		t.Fatalf("post grn: %v", err)
	}
	if _, err := pos.Close(ctx, invTenantA, closed.ID, invBuyer); err != nil {
		t.Fatalf("close: %v", err)
	}
	err = create(CreateInput{POID: closed.ID, Number: "INV-G-3",
		Items: []Item{{POItemID: closedLaptop.ID, Qty: 2, UnitPriceCents: 8000000}}, Actor: invFinance})
	var over *OverBillError
	if !errors.As(err, &over) || errors.Is(err, purchaseorder.ErrIllegalTransition) {
		t.Fatalf("closed over-budget err = %v, want the typed over-bill answer", err)
	}

	// A fitting invoice on the closed PO answers the status guard instead.
	err = create(CreateInput{POID: closed.ID, Number: "INV-G-4",
		Items: []Item{{POItemID: closedLaptop.ID, Qty: 1, UnitPriceCents: 100000}}, Actor: invFinance})
	if !errors.Is(err, purchaseorder.ErrIllegalTransition) || err.Error() != "po_illegal_transition" {
		t.Fatalf("closed fitting err = %v, want po_illegal_transition", err)
	}

	// Nothing was written by any guard.
	if n := invCount(t, db, invTenantA); n != 0 {
		t.Fatalf("invoices after all guards = %d, want 0", n)
	}

	// Cross-tenant denial: tenant B cannot even see tenant A's PO.
	_, err = s.Create(ctx, invTenantB, CreateInput{POID: po.ID, Number: "INV-B-1",
		Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 1}}, Actor: invFinance})
	if !errors.Is(err, purchaseorder.ErrNotFound) {
		t.Fatalf("cross-tenant create err = %v, want ErrNotFound", err)
	}
}

// AC (#39 duplicates + reads): the same (tenant, vendor, number) is a
// duplicate; the same number under a different vendor is a different
// invoice. Get hides other tenants' invoices behind the same not-found.
func TestStoreCreateInvoiceDuplicateAndRead(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	poA := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	poB := invoicablePO(t, db, invTenantA, "INV-39-B", invBasket) // different vendor
	laptopA, laptopB := invItemByQty(t, poA, 2), invItemByQty(t, poB, 2)

	if _, err := s.Create(ctx, invTenantA, CreateInput{
		POID: poA.ID, Number: "SHARED-NUM",
		Items: []Item{{POItemID: laptopA.ID, Qty: 1, UnitPriceCents: 100000}}, Actor: invFinance,
	}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	// Same vendor, same number → duplicate (UNIQUE index).
	_, err := s.Create(ctx, invTenantA, CreateInput{
		POID: poA.ID, Number: "SHARED-NUM",
		Items: []Item{{POItemID: laptopA.ID, Qty: 1, UnitPriceCents: 100000}}, Actor: invFinance,
	})
	if !errors.Is(err, ErrNumberDuplicate) || err.Error() != "inv_number_duplicate" {
		t.Fatalf("duplicate err = %v, want inv_number_duplicate", err)
	}
	// Same number, different vendor → accepted.
	other, err := s.Create(ctx, invTenantA, CreateInput{
		POID: poB.ID, Number: "SHARED-NUM",
		Items: []Item{{POItemID: laptopB.ID, Qty: 1, UnitPriceCents: 100000}}, Actor: invFinance,
	})
	if err != nil {
		t.Fatalf("same number other vendor: %v", err)
	}

	// Reads: malformed id, missing id, and the cross-tenant 404 twin.
	if _, err := s.Get(ctx, invTenantA, "nope"); !errors.Is(err, ErrIDInvalid) {
		t.Fatalf("get bad id err = %v, want ErrIDInvalid", err)
	}
	if _, err := s.Get(ctx, invTenantA, "99999999-0000-0000-0000-0000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, invTenantB, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get err = %v, want ErrNotFound (indistinguishable)", err)
	}
}

// draftPO converts a fresh approved requisition and stops at draft.
func draftPO(t *testing.T, db *pgxpool.Pool, tenantID string) purchaseorder.PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	basket := []requisition.Item{{Description: "Keyboard", Qty: 5, UnitPriceCents: 400000, Category: "it"}}
	if _, err := approval.NewStore(db).ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: invRequester, Currency: "IDR", Items: basket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, invRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
		_, err := apps.MaterializeTx(ctx, tx, req)
		return err
	}); err != nil {
		t.Fatalf("submit requisition: %v", err)
	}
	steps, err := apps.ListSteps(ctx, tenantID, r.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("list steps = (%+v, %v), want one", steps, err)
	}
	if _, _, err := apps.Decide(ctx, tenantID, approval.DecideInput{
		StepID: steps[0].ID, Decision: approval.DecisionApprove,
		ActorID: invApprover, Roles: []string{"approver"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	v, err := vendor.NewStore(db).Create(ctx, tenantID, vendor.CreateInput{
		Code: "INV-39-DR", Name: "PT Invoice Draft", UserID: invBuyer,
	})
	if err != nil {
		t.Fatalf("create draft vendor: %v", err)
	}
	po, _, err := purchaseorder.NewStore(db).CreateFromRequisition(ctx, tenantID, r.ID, v.ID, invBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// The conversion response carries no item ids; load them directly.
	var items []purchaseorder.Item
	if err := tenant.WithTenant(ctx, db, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, description, qty_ordered, qty_received, unit_price_cents
			FROM procurement.po_items WHERE po_id = $1 ORDER BY created_at, id`, po.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it purchaseorder.Item
			if err := rows.Scan(&it.ID, &it.Description, &it.Qty, &it.QtyReceived, &it.UnitPriceCents); err != nil {
				return err
			}
			items = append(items, it)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load draft items: %v", err)
	}
	po.Items = items
	return po
}

// errString renders nil errors as "<nil>" for table messages.
func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// mustValidateInv normalizes a filter or fails the test.
func mustValidateInv(t *testing.T, f ListFilter) ListFilter {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	return filter
}

// listOK runs one validated list call, failing the test on error.
func listOK(t *testing.T, s *Store, tenantID string, f ListFilter) ([]Invoice, int) {
	t.Helper()
	rows, total, err := s.List(t.Context(), tenantID, mustValidateInv(t, f))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return rows, total
}

// setInvStatus is the list fixture's SQL half: only the #40 match writes
// matched/discrepancy + match_result, and pulling the whole match engine in
// would invert this package's dependency direction — the list only needs the
// stored values, so the fixture sets them directly under RLS bypass.
func setInvStatus(t *testing.T, db *pgxpool.Pool, invoiceID, status, matchResult string) {
	t.Helper()
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			UPDATE procurement.invoices
			SET status = $2, match_result = NULLIF($3, '')::jsonb
			WHERE id = $1`, invoiceID, status, matchResult)
		return err
	}); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

// AC (#41 list filters): descending creation order, header-only rows (no
// lines, no match_result — even on invoices whose stored match_result is
// filled), status equality over the three-value whitelist, case-insensitive
// number-prefix matching with literal wildcards, combined filters, and
// cross-tenant invisibility through RLS.
func TestStoreListFilters(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	laptop := invItemByQty(t, po, 2)

	// Three invoices of one vendor (distinct numbers, 3 × 5,000,000 fits the
	// 15,450,000 budget), mixed-case numbers for the prefix arm.
	create := func(number string) Invoice {
		t.Helper()
		inv, err := s.Create(ctx, invTenantA, CreateInput{
			POID: po.ID, Number: number,
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: 5000000}},
			Actor: invFinance,
		})
		if err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
		return inv
	}
	first := create("inv/lowercase/001")
	second := create("INV/MIXED/002")
	third := create("Inv/Mixed/003")
	// The match is the only writer of matched/discrepancy — the fixture sets
	// the stored verdicts directly (see setInvStatus).
	setInvStatus(t, db, second.ID, StatusDiscrepancy, `{"matched":false,"lines":[]}`)
	setInvStatus(t, db, third.ID, StatusMatched, `{"matched":true,"lines":[]}`)

	// Tenant B's own PO + invoice — the cross-tenant row.
	poB := invoicablePO(t, db, invTenantB, "INV-39-B2", invBasket)
	laptopB := invItemByQty(t, poB, 2)
	if _, err := s.Create(ctx, invTenantB, CreateInput{
		POID: poB.ID, Number: "INV-TENANT-B",
		Items: []Item{{POItemID: laptopB.ID, Qty: 1, UnitPriceCents: 5000000}},
		Actor: invFinance,
	}); err != nil {
		t.Fatalf("create tenant B invoice: %v", err)
	}

	// Defaults: tenant A's three rows newest first, header-only.
	rows, total := listOK(t, s, invTenantA, ListFilter{PageSize: 10})
	if total != 3 || len(rows) != 3 {
		t.Fatalf("defaults: total = %d, rows = %d, want 3/3", total, len(rows))
	}
	if rows[0].ID != third.ID || rows[1].ID != second.ID || rows[2].ID != first.ID {
		t.Fatalf("not newest-first: %s, %s, %s", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	for _, row := range rows {
		if row.Items != nil {
			t.Fatalf("list row carries lines: %+v", row.Items)
		}
		if row.MatchResult != "" {
			t.Fatalf("list row carries match_result: %q", row.MatchResult)
		}
		if row.Number == "" || row.VendorID == "" || row.POID == "" || row.Currency == "" ||
			row.TotalCents == 0 || row.Status == "" || row.CreatedAt.IsZero() {
			t.Fatalf("header fields missing: %+v", row)
		}
	}

	// Status filters over the three-value whitelist.
	rows, total = listOK(t, s, invTenantA, ListFilter{Status: StatusMatched, PageSize: 10})
	if total != 1 || len(rows) != 1 || rows[0].ID != third.ID {
		t.Fatalf("matched filter: (%d, %d rows, first %s)", total, len(rows), rows[0].ID)
	}
	rows, total = listOK(t, s, invTenantA, ListFilter{Status: StatusDiscrepancy, PageSize: 10})
	if total != 1 || rows[0].ID != second.ID {
		t.Fatalf("discrepancy filter: (%d, %s)", total, rows[0].ID)
	}
	rows, total = listOK(t, s, invTenantA, ListFilter{Status: StatusPending, PageSize: 10})
	if total != 1 || rows[0].ID != first.ID {
		t.Fatalf("pending filter: (%d, %s)", total, rows[0].ID)
	}

	// Number-prefix query, lowercase input over mixed-case numbers.
	rows, total = listOK(t, s, invTenantA, ListFilter{Query: "inv/lower", PageSize: 10})
	if total != 1 || rows[0].ID != first.ID {
		t.Fatalf("prefix lowercase: (%d, %s), want first", total, rows[0].ID)
	}
	// A literal % matches nothing (wildcards escaped by ValidateList).
	rows, total = listOK(t, s, invTenantA, ListFilter{Query: "%", PageSize: 10})
	if total != 0 || len(rows) != 0 {
		t.Fatalf("literal %%: (%d, %d rows), want 0/0", total, len(rows))
	}
	// Combined: the shared prefix + matched leaves only third.
	rows, total = listOK(t, s, invTenantA, ListFilter{Query: "INV/", Status: StatusMatched, PageSize: 10})
	if total != 1 || rows[0].ID != third.ID {
		t.Fatalf("combined: (%d, %s), want third", total, rows[0].ID)
	}

	// Cross-tenant: tenant B's list sees only its own row, never tenant A's;
	// tenant A's totals never count tenant B's invoice.
	rows, total = listOK(t, s, invTenantB, ListFilter{})
	if total != 1 || len(rows) != 1 || rows[0].TenantID != invTenantB ||
		rows[0].Number != "INV-TENANT-B" {
		t.Fatalf("tenant B list = (%d, %+v), want its one row only", total, rows)
	}
	rows, total = listOK(t, s, invTenantA, ListFilter{Query: "INV-TENANT-B"})
	if total != 0 || len(rows) != 0 {
		t.Fatalf("tenant A sees tenant B's invoice: (%d, %+v)", total, rows)
	}
}

// AC (#41 pagination): deep pages slice correctly, a past-the-end page keeps
// the true total through the COUNT fallback, and zero values normalize to the
// 1/20 defaults.
func TestStoreListPagination(t *testing.T) {
	db := openTestDB(t)
	cleanupINV(t, db)
	t.Cleanup(func() { cleanupINV(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, invTenantA, "INV-39-A", invBasket)
	laptop := invItemByQty(t, po, 2)

	var created []Invoice
	for i, number := range []string{"INV-PAGE-1", "INV-PAGE-2", "INV-PAGE-3"} {
		inv, err := s.Create(ctx, invTenantA, CreateInput{
			POID: po.ID, Number: number,
			Items: []Item{{POItemID: laptop.ID, Qty: 1, UnitPriceCents: int64(i+1) * 100000}},
			Actor: invFinance,
		})
		if err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
		created = append(created, inv)
	}

	// Deep page: page 2 of size 2 carries the oldest row only.
	rows, total := listOK(t, s, invTenantA, ListFilter{Page: 2, PageSize: 2})
	if total != 3 || len(rows) != 1 || rows[0].ID != created[0].ID {
		t.Fatalf("deep page: total = %d, rows = %+v", total, rows)
	}
	// Past-the-end page: no rows, but the true total via the fallback COUNT.
	rows, total = listOK(t, s, invTenantA, ListFilter{Page: 5, PageSize: 2})
	if total != 3 || len(rows) != 0 {
		t.Fatalf("past-the-end: total = %d, rows = %d, want 3/0", total, len(rows))
	}
	// Zero values: the normalized 1/20 defaults return everything.
	rows, total = listOK(t, s, invTenantA, ListFilter{})
	if total != 3 || len(rows) != 3 {
		t.Fatalf("defaults: total = %d, rows = %d, want 3/3", total, len(rows))
	}
	if rows[0].ID != created[2].ID {
		t.Fatalf("defaults not newest-first: %s", rows[0].ID)
	}
}
