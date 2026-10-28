package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	mtTenantA   = "99999999-0000-0000-0000-000000000001"
	mtTenantB   = "99999999-0000-0000-0000-000000000002"
	mtRequester = "99999999-0000-0000-0000-0000000000aa"
	mtApprover  = "99999999-0000-0000-0000-0000000000bb"
	mtBuyer     = "99999999-0000-0000-0000-0000000000cc"
	mtFinance   = "99999999-0000-0000-0000-0000000000dd"
)

// openTestDB mirrors the invoice/goodsreceipt harness: apply migrations,
// open a pool, skip without TEST_DATABASE_URL.
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

// cleanupMT wipes everything the match tests create in the shared test
// tenants (the invoice harness's tenant set, plus match_settings).
func cleanupMT(t *testing.T, db *pgxpool.Pool) {
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
			`DELETE FROM procurement.match_settings WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'MT-40-%'`,
		} {
			if _, err := tx.Exec(ctx, stmt, mtTenantA, mtTenantB); err != nil {
				return err
			}
		}
		return nil
	})
}

// mtBasket is the standard fixture: one line, qty 100 @ 1000 (total
// 100,000) — the cumulative tests bill 40 + 40 against a receipt of 60.
var mtBasket = []requisition.Item{
	{Description: "Fixture line", Qty: 100, UnitPriceCents: 1000, Category: "it"},
}

// invoicablePO drives the real flow — rules, requisition, submit, approve,
// convert, issue, acknowledge — so every match lands on a PO the domain
// state machine actually walked here (the invoice harness's twin).
func invoicablePO(t *testing.T, db *pgxpool.Pool, tenantID, vendorCode string, basket []requisition.Item) purchaseorder.PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	if _, err := approval.NewStore(db).ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	v, err := vendor.NewStore(db).Create(ctx, tenantID, vendor.CreateInput{
		Code: vendorCode, Name: "PT Match Fixture", UserID: mtBuyer,
	})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: mtRequester, Currency: "IDR", Items: basket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, mtRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
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
		ActorID: mtApprover, Roles: []string{"approver"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	pos := purchaseorder.NewStore(db)
	po, _, err := pos.CreateFromRequisition(ctx, tenantID, r.ID, v.ID, mtBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, err := pos.Issue(ctx, tenantID, po.ID, mtBuyer); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ack, err := pos.Acknowledge(ctx, tenantID, po.ID, mtBuyer)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	return ack
}

// createInvoice records one vendor invoice against the PO.
func createInvoice(t *testing.T, db *pgxpool.Pool, tenantID string, po purchaseorder.PurchaseOrder, number string, qty int32, price int64) invoice.Invoice {
	t.Helper()
	inv, err := invoice.NewStore(db).Create(context.Background(), tenantID, invoice.CreateInput{
		POID: po.ID, Number: number,
		Items: []invoice.Item{{POItemID: po.Items[0].ID, Qty: qty, UnitPriceCents: price}},
		Actor: mtFinance,
	})
	if err != nil {
		t.Fatalf("create invoice %s: %v", number, err)
	}
	return inv
}

// postGRN posts one receipt line against the PO's first item.
func postGRN(t *testing.T, db *pgxpool.Pool, tenantID string, po purchaseorder.PurchaseOrder, qty int32) {
	t.Helper()
	if _, _, err := goodsreceipt.NewStore(db).Post(context.Background(), tenantID, goodsreceipt.PostInput{
		POID: po.ID, Items: []goodsreceipt.Item{{POItemID: po.Items[0].ID, QtyReceived: qty}}, Actor: mtBuyer,
	}); err != nil {
		t.Fatalf("post grn: %v", err)
	}
}

// poAudit reads the purchase_orders columns a match run must not move.
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

// outboxRows returns the tenant's invoice.* outbox rows in creation order.
func outboxRows(t *testing.T, db *pgxpool.Pool, tenantID string) []struct {
	EventType   string
	EntityType  string
	EntityID    string
	PayloadJSON string
} {
	t.Helper()
	var out []struct {
		EventType   string
		EntityType  string
		EntityID    string
		PayloadJSON string
	}
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `
			SELECT event_type, entity_type, entity_id::text, payload::text
			FROM procurement.outbox
			WHERE tenant_id = $1 AND event_type LIKE 'invoice.%'
			ORDER BY occurred_at, id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r struct {
				EventType   string
				EntityType  string
				EntityID    string
				PayloadJSON string
			}
			if err := rows.Scan(&r.EventType, &r.EntityType, &r.EntityID, &r.PayloadJSON); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// storedMatchResult reads the persisted match_result of one invoice.
// Note the column is jsonb: PostgreSQL stores the canonical form (its own
// key order and spacing), so byte-level comparisons against the engine's
// ResultJSON are semantic (sameJSON), while before/after comparisons of
// the column itself stay byte-exact — the canonical form is deterministic
// too.
func storedMatchResult(t *testing.T, db *pgxpool.Pool, invoiceID string) string {
	t.Helper()
	var result *string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT match_result::text FROM procurement.invoices WHERE id = $1`, invoiceID).Scan(&result)
	}); err != nil {
		t.Fatalf("read match_result: %v", err)
	}
	if result == nil {
		return ""
	}
	return *result
}

// sameJSON compares two JSON documents semantically (unmarshal both).
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("sameJSON: parse %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("sameJSON: parse %q: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

// AC (happy path): a clean invoice matches — status moves pending →
// matched, match_result holds the per-line detail, exactly one
// invoice.matched outbox row commits (entity_type invoice), and the
// purchase_orders row is untouched.
func TestStoreMatchHappyPath(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, mtTenantA, "MT-40-A", mtBasket)
	postGRN(t, db, mtTenantA, po, 60)
	inv := createInvoice(t, db, mtTenantA, po, "MT/2026/0001", 60, 1000)

	beforeStatus, beforeUpdated, beforeBy := poAudit(t, db, po.ID)
	updated, result, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !result.Matched || updated.Status != invoice.StatusMatched {
		t.Fatalf("verdict = (%v, %s), want matched", result.Matched, updated.Status)
	}
	if !sameJSON(t, updated.MatchResult, storedMatchResult(t, db, inv.ID)) {
		t.Fatalf("returned match_result = %q, stored = %q", updated.MatchResult, storedMatchResult(t, db, inv.ID))
	}
	want := fmt.Sprintf(`{"matched":true,"lines":[{"po_item_id":"%s","qty_ordered":100,"qty_received":60,`+
		`"qty_invoiced":60,"po_unit_price_cents":1000,"invoice_unit_price_cents":1000,`+
		`"qty_breach":false,"price_breach":false,"reason":""}]}`, po.Items[0].ID)
	if updated.MatchResult != want {
		t.Fatalf("match_result =\n%s\nwant\n%s", updated.MatchResult, want)
	}
	rows := outboxRows(t, db, mtTenantA)
	if len(rows) != 1 || rows[0].EventType != OutboxMatched ||
		rows[0].EntityType != "invoice" || rows[0].EntityID != inv.ID {
		t.Fatalf("outbox rows = %+v, want one invoice.matched on the invoice", rows)
	}
	if afterStatus, afterUpdated, afterBy := poAudit(t, db, po.ID); afterStatus != beforeStatus ||
		afterUpdated != beforeUpdated || afterBy != beforeBy {
		t.Fatalf("match moved purchase_orders: (%s,%s,%s) -> (%s,%s,%s)",
			beforeStatus, beforeUpdated, beforeBy, afterStatus, afterUpdated, afterBy)
	}
	// The re-run guard: matched is final.
	if _, _, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance); !errors.Is(err, invoice.ErrAlreadyMatched) {
		t.Fatalf("re-run err = %v, want inv_already_matched", err)
	}
	if rows := outboxRows(t, db, mtTenantA); len(rows) != 1 {
		t.Fatalf("rejected re-run appended %d extra rows, want 0", len(rows)-1)
	}
}

// AC (discrepancy + resolution loop): an over-invoiced line flags
// discrepancy with the pinned reason; a late GRN resolves it on a re-run
// (each run appends its own event); an unchanged re-run stores byte-ident
// ical match_result bytes; matched afterwards rejects further runs.
func TestStoreMatchDiscrepancyAndRerun(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, mtTenantA, "MT-40-B", mtBasket)
	inv := createInvoice(t, db, mtTenantA, po, "MT/2026/0002", 10, 1000)

	_, result, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if result.Matched {
		t.Fatalf("billed line without any receipt must flag")
	}
	if len(result.Lines) != 1 || !result.Lines[0].QtyBreach {
		t.Fatalf("lines = %+v", result.Lines)
	}
	wantReason := fmt.Sprintf("inv_over_bill_qty: po_item=%s invoiced=10 received=0 pct=0", po.Items[0].ID)
	if result.Lines[0].Reason != wantReason {
		t.Fatalf("reason = %q, want %q", result.Lines[0].Reason, wantReason)
	}
	firstResult := storedMatchResult(t, db, inv.ID)

	// An unchanged re-run is legal (status discrepancy) and stores the
	// byte-identical result — same state, same bytes.
	_, result2, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance)
	if err != nil {
		t.Fatalf("re-match on discrepancy: %v", err)
	}
	if result2.Matched || storedMatchResult(t, db, inv.ID) != firstResult {
		t.Fatalf("unchanged re-run bytes differ:\n%s\n%s", storedMatchResult(t, db, inv.ID), firstResult)
	}

	// The late GRN: the full receipt arrives, the re-run resolves to
	// matched — history stays append-only (three events by now).
	postGRN(t, db, mtTenantA, po, 100)
	_, result3, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance)
	if err != nil {
		t.Fatalf("resolve re-run: %v", err)
	}
	if !result3.Matched || storedMatchResult(t, db, inv.ID) == firstResult {
		t.Fatalf("resolution did not move the verdict: %+v", result3)
	}
	rows := outboxRows(t, db, mtTenantA)
	if len(rows) != 3 {
		t.Fatalf("outbox rows = %d (%+v), want 3 (discrepancy, discrepancy, matched)", len(rows), rows)
	}
	if rows[0].EventType != OutboxDiscrepancy || rows[1].EventType != OutboxDiscrepancy ||
		rows[2].EventType != OutboxMatched {
		t.Fatalf("event order = %v", rows)
	}
	if _, _, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance); !errors.Is(err, invoice.ErrAlreadyMatched) {
		t.Fatalf("post-match err = %v, want inv_already_matched", err)
	}
}

// AC (cumulative basis + progress billing + tolerance surface): with a
// receipt of 60, a lone 40-invoice matches (progress billing is not a
// breach); once a second 40-invoice exists, the SECOND match flags — the
// PO's cumulative invoiced (80) exceeds the received (60), even though
// each invoice alone is reasonable; raising the tenant's qty tolerance to
// 34% resolves the discrepancy on a re-run.
func TestStoreMatchCumulativeBasisAndSettings(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, mtTenantA, "MT-40-C", mtBasket)
	postGRN(t, db, mtTenantA, po, 60)
	first := createInvoice(t, db, mtTenantA, po, "MT/2026/0003", 40, 1000)

	if _, result, err := s.Match(ctx, mtTenantA, first.ID, mtFinance); err != nil || !result.Matched {
		t.Fatalf("progress billing first invoice = (%+v, %v), want matched", result, err)
	}
	second := createInvoice(t, db, mtTenantA, po, "MT/2026/0004", 40, 1000)
	_, result, err := s.Match(ctx, mtTenantA, second.ID, mtFinance)
	if err != nil {
		t.Fatalf("match second: %v", err)
	}
	if result.Matched || !result.Lines[0].QtyBreach || result.Lines[0].QtyInvoiced != 80 {
		t.Fatalf("cumulative verdict = %+v, want breach with qty_invoiced 80", result)
	}

	// The tenant's tolerance moves the boundary: 80 <= 60 x 1.34.
	if _, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: 34, PricePct: 0}, mtFinance); err != nil {
		t.Fatalf("replace settings: %v", err)
	}
	_, resolved, err := s.Match(ctx, mtTenantA, second.ID, mtFinance)
	if err != nil {
		t.Fatalf("re-match with tolerance: %v", err)
	}
	if !resolved.Matched {
		t.Fatalf("tolerance 34pct did not absorb the cumulative overrun: %+v", resolved)
	}
}

// AC (price): the symmetric price guard flags both directions against the
// PO price, and the boundary equality sits inside. Each case gets its own
// PO — a +15% price on the full quantity would also overshoot
// po.total_cents at create (the #39 strict guard), so every invoice bills
// half the ordered quantity.
func TestStoreMatchPriceSymmetric(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	if _, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: 0, PricePct: 10}, mtFinance); err != nil {
		t.Fatalf("replace settings: %v", err)
	}
	cases := []struct {
		vendorCode string
		number     string
		price      int64
		wantMatch  bool
	}{
		{"MT-40-D1", "MT/2026/0005", 1150, false}, // +15% → breach
		{"MT-40-D2", "MT/2026/0006", 1100, true},  // exactly +10% → within
		{"MT-40-D3", "MT/2026/0007", 850, false},  // -15% → breach
	}
	for _, tc := range cases {
		po := invoicablePO(t, db, mtTenantA, tc.vendorCode, mtBasket)
		postGRN(t, db, mtTenantA, po, 100)
		inv := createInvoice(t, db, mtTenantA, po, tc.number, 50, tc.price)
		_, result, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance)
		if err != nil {
			t.Fatalf("%s: match: %v", tc.number, err)
		}
		if result.Matched != tc.wantMatch || result.Lines[0].PriceBreach == tc.wantMatch {
			t.Fatalf("%s price %d verdict = %+v, want matched=%v", tc.number, tc.price, result.Lines[0], tc.wantMatch)
		}
		if result.Lines[0].QtyBreach {
			t.Fatalf("%s: price case tripped the qty guard: %+v", tc.number, result.Lines[0])
		}
	}
}

// AC (guards): non-UUID ids fail fast; missing and cross-tenant ids are
// indistinguishable (inv_not_found); nothing is written.
func TestStoreMatchGuards(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, mtTenantA, "MT-40-E", mtBasket)
	postGRN(t, db, mtTenantA, po, 100)
	inv := createInvoice(t, db, mtTenantA, po, "MT/2026/0009", 100, 1000)

	if _, _, err := s.Match(ctx, mtTenantA, "not-a-uuid", mtFinance); !errors.Is(err, invoice.ErrIDInvalid) {
		t.Fatalf("bad id err = %v, want inv_id_invalid", err)
	}
	if _, _, err := s.Match(ctx, mtTenantA, "11111111-0000-0000-0000-0000000000ff", mtFinance); !errors.Is(err, invoice.ErrNotFound) {
		t.Fatalf("missing id err = %v, want inv_not_found", err)
	}
	if _, _, err := s.Match(ctx, mtTenantB, inv.ID, mtFinance); !errors.Is(err, invoice.ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want inv_not_found (indistinguishable)", err)
	}
	if inv.Status != invoice.StatusPending || storedMatchResult(t, db, inv.ID) != "" {
		t.Fatalf("guarded runs moved the invoice: %s %q", inv.Status, storedMatchResult(t, db, inv.ID))
	}
	if rows := outboxRows(t, db, mtTenantA); len(rows) != 0 {
		t.Fatalf("guarded runs appended %d rows, want 0", len(rows))
	}
}

// AC (concurrency): two parallel matches of one invoice serialize on the
// PO row lock — exactly one commits, the loser answers
// inv_already_matched, and exactly one event exists.
func TestStoreMatchConcurrency(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := invoicablePO(t, db, mtTenantA, "MT-40-F", mtBasket)
	postGRN(t, db, mtTenantA, po, 100)
	inv := createInvoice(t, db, mtTenantA, po, "MT/2026/0010", 100, 1000)

	var wg sync.WaitGroup
	var ok, rejected atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Match(ctx, mtTenantA, inv.ID, mtFinance); err == nil {
				ok.Add(1)
			} else if errors.Is(err, invoice.ErrAlreadyMatched) {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf("parallel matches = (%d ok, %d rejected), want (1, 1)", ok.Load(), rejected.Load())
	}
	if rows := outboxRows(t, db, mtTenantA); len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want exactly one", len(rows))
	}
}

// AC (settings): the read answers the strict defaults while no row exists;
// Replace validates 0..100 (machine codes), stores, and echoes; RLS keeps
// tenants apart — tenant B's row is invisible to tenant A, whose effective
// settings stay the defaults.
func TestStoreSettings(t *testing.T) {
	db := openTestDB(t)
	cleanupMT(t, db)
	t.Cleanup(func() { cleanupMT(t, db) })
	ctx := context.Background()
	s := NewStore(db)

	if got, err := s.GetSettings(ctx, mtTenantA); err != nil || got != (Settings{QtyPct: 0, PricePct: 0}) {
		t.Fatalf("default settings = (%+v, %v), want 0/0", got, err)
	}
	if _, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: -1, PricePct: 0}, mtFinance); !errors.Is(err, ErrQtyPctInvalid) {
		t.Fatalf("qty pct err = %v, want match_qty_pct_invalid", err)
	}
	if _, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: 0, PricePct: 101}, mtFinance); !errors.Is(err, ErrPricePctInvalid) {
		t.Fatalf("price pct err = %v, want match_price_pct_invalid", err)
	}
	stored, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: 5, PricePct: 2}, mtFinance)
	if err != nil || stored != (Settings{QtyPct: 5, PricePct: 2}) {
		t.Fatalf("replace = (%+v, %v)", stored, err)
	}
	again, err := s.ReplaceSettings(ctx, mtTenantA, Settings{QtyPct: 7, PricePct: 3}, mtFinance)
	if err != nil || again != (Settings{QtyPct: 7, PricePct: 3}) {
		t.Fatalf("second replace = (%+v, %v), want upsert", again, err)
	}
	if got, err := s.GetSettings(ctx, mtTenantA); err != nil || got != (Settings{QtyPct: 7, PricePct: 3}) {
		t.Fatalf("get after replace = (%+v, %v)", got, err)
	}
	// RLS: tenant B sees its own defaults, not A's row.
	if got, err := s.GetSettings(ctx, mtTenantB); err != nil || got != (Settings{QtyPct: 0, PricePct: 0}) {
		t.Fatalf("tenant B settings = (%+v, %v), want its own 0/0", got, err)
	}
	if _, err := s.ReplaceSettings(ctx, mtTenantB, Settings{QtyPct: 50, PricePct: 50}, mtFinance); err != nil {
		t.Fatalf("tenant B replace: %v", err)
	}
	if got, _ := s.GetSettings(ctx, mtTenantA); got != (Settings{QtyPct: 7, PricePct: 3}) {
		t.Fatalf("tenant B write leaked into A: %+v", got)
	}
}
