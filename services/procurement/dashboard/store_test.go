package dashboard

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	dashTenantA   = "77777777-0052-0000-0000-000000000001"
	dashTenantB   = "77777777-0052-0000-0000-000000000002"
	dashRequester = "77777777-0052-0000-0000-0000000000aa"
)

// openTestDB mirrors the invoice harness: apply migrations, open a pool,
// skip without TEST_DATABASE_URL.
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

// cleanupDash wipes everything the dashboard tests create in the shared test
// tenants: outbox rows, POs (items cascade), PO counters, requisitions
// (items/steps cascade), PR counters, approval rules, and the test vendors —
// the invoice harness order.
func cleanupDash(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		ctx := context.Background()
		for _, stmt := range []string{
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.purchase_orders WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.po_counters WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.requisitions WHERE tenant_id IN ($1, $2) AND number LIKE 'PR-%'`,
			`DELETE FROM procurement.requisition_counters WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.approval_rules WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'DASH-52-%'`,
		} {
			if _, err := tx.Exec(ctx, stmt, dashTenantA, dashTenantB); err != nil {
				return err
			}
		}
		return nil
	})
}

// dashVendor creates one fixture vendor.
func dashVendor(t *testing.T, db *pgxpool.Pool, tenantID, code, name string) vendor.Vendor {
	t.Helper()
	v, err := vendor.NewStore(db).Create(context.Background(), tenantID, vendor.CreateInput{
		Code: code, Name: name, UserID: dashRequester,
	})
	if err != nil {
		t.Fatalf("create vendor %s: %v", code, err)
	}
	return v
}

// dashRequisition drives the real domain (create → submit materializes the
// rule's steps) so the pending queue holds genuine materialized steps.
func dashRequisition(t *testing.T, db *pgxpool.Pool, tenantID string, items []requisition.Item) requisition.Requisition {
	t.Helper()
	ctx := context.Background()
	r, err := requisition.NewStore(db).Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: dashRequester, Currency: "IDR", Items: items,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	r, err = requisition.NewStore(db).Submit(ctx, tenantID, r.ID, dashRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
		_, err := approval.NewStore(db).MaterializeTx(ctx, tx, req)
		return err
	})
	if err != nil {
		t.Fatalf("submit requisition: %v", err)
	}
	return r
}

// setSubmittedAt pins a requisition's submitted_at under RLS bypass so the
// oldest-first order of the pending queue is deterministic (the domain
// stamps wall-clock now, which consecutive submits can share).
func setSubmittedAt(t *testing.T, db *pgxpool.Pool, requisitionID string, at time.Time) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`UPDATE procurement.requisitions SET submitted_at = $2 WHERE id = $1`, requisitionID, at)
		return err
	})
}

// closeRequisition moves a fixture requisition to closed under RLS bypass —
// the state PO conversion leaves behind — so a PO-carrying parent stops
// counting as a pending approval (the read model's status guard).
func closeRequisition(t *testing.T, db *pgxpool.Pool, requisitionID string) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`UPDATE procurement.requisitions SET status = 'closed' WHERE id = $1`, requisitionID)
		return err
	})
}

// poFixture inserts one PO straight through SQL (the #41 EXPLAIN-fixture
// precedent: the read model needs controlled issued_at values the domain's
// Issue stamps with wall-clock now). One PO per requisition (the 0007
// UNIQUE); issuedAt nil means a draft that was never issued.
func poFixture(t *testing.T, db *pgxpool.Pool, tenantID, requisitionID, vendorID string, issuedAt *time.Time, status, currency string, totalCents int64) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO procurement.purchase_orders
				(tenant_id, number, requisition_id, vendor_id, status, currency, total_cents, issued_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			tenantID, "PO-"+requisitionID[:8], requisitionID, vendorID,
			status, currency, totalCents, issuedAt, dashRequester)
		return err
	})
}

var dashBasket = []requisition.Item{
	{Description: "Fixture line", Qty: 1, UnitPriceCents: 100, Category: "test"},
}

// TestStoreSummaryReconciles is the #52 reconciliation AC on a hand-shaped
// fixture: the summary's pending count equals the current pending steps the
// approval read sees, the PO status distribution totals the unfiltered PO
// list, and the spend rows equal the per-vendor+currency sums over the PO
// list rows inside each window (issued_at in window, status neither draft
// nor cancelled — the #49 register basis). The queue also proves the first
// page bound (seven pending, five shown, oldest first).
func TestStoreSummaryReconciles(t *testing.T) {
	db := openTestDB(t)
	cleanupDash(t, db)
	t.Cleanup(func() { cleanupDash(t, db) })
	ctx := context.Background()
	s := NewStore(db)

	if _, err := approval.NewStore(db).ReplaceRules(ctx, dashTenantA, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
		{Currency: "IDR", StepNo: 2, RoleRequired: "admin", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}

	vX := dashVendor(t, db, dashTenantA, "DASH-52-X", "PT Spend X")
	vY := dashVendor(t, db, dashTenantA, "DASH-52-Y", "PT Spend Y")

	now := time.Now().UTC().Truncate(time.Second)

	// Seven submitted requisitions with pending steps, submitted 7h..1h ago
	// — the queue must answer total 7 with the first page (5) oldest-first,
	// each row the current step (the smallest pending step_no = 1) with its
	// requisition header.
	var submitted []requisition.Requisition
	for i := range 7 {
		r := dashRequisition(t, db, dashTenantA, dashBasket)
		setSubmittedAt(t, db, r.ID, now.Add(-time.Duration(7-i)*time.Hour))
		submitted = append(submitted, r)
	}

	// POs across the windows, statuses, vendors, and currencies. Each parent
	// is closed afterwards — the state conversion leaves — so only the seven
	// above sit in the approval queue.
	tenDays := now.Add(-10 * 24 * time.Hour)
	fortyFiveDays := now.Add(-45 * 24 * time.Hour)
	hundredDays := now.Add(-100 * 24 * time.Hour)
	for _, tc := range []struct {
		vendor   vendor.Vendor
		issuedAt *time.Time // nil = draft, never issued
		status   string
		curr     string
		cents    int64
	}{
		{vX, &tenDays, "issued", "IDR", 1_000_000},
		{vX, &fortyFiveDays, "acknowledged", "IDR", 2_000_000},
		{vX, &tenDays, "received", "USD", 500},
		{vY, &hundredDays, "closed", "IDR", 4_000_000},
		{vY, &tenDays, "cancelled", "IDR", 8_000_000}, // issued but cancelled: excluded from spend
		{vY, nil, "draft", "IDR", 16_000_000},         // never issued: excluded from spend
	} {
		parent := dashRequisition(t, db, dashTenantA, dashBasket)
		poFixture(t, db, dashTenantA, parent.ID, tc.vendor.ID, tc.issuedAt, tc.status, tc.curr, tc.cents)
		closeRequisition(t, db, parent.ID)
	}

	summary, err := s.Summary(ctx, dashTenantA, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	// --- Pending approvals: count reconciles with the approval read ---
	steps := approval.NewStore(db)
	wantPending := 0
	for _, r := range submitted {
		got, err := steps.ListSteps(ctx, dashTenantA, r.ID)
		if err != nil {
			t.Fatalf("list steps: %v", err)
		}
		for _, st := range got {
			if st.Status == "pending" {
				wantPending++ // one current pending step per submitted requisition
				break
			}
		}
	}
	if wantPending != 7 || summary.Pending.Total != wantPending {
		t.Fatalf("pending total = %d, want %d (reconciled with ListSteps over the 7 submitted)", summary.Pending.Total, wantPending)
	}
	if len(summary.Pending.Items) != PendingPageSize {
		t.Fatalf("pending items = %d, want the first page of %d", len(summary.Pending.Items), PendingPageSize)
	}
	for i, item := range summary.Pending.Items {
		want := submitted[i] // submitted_at pinned descending in age → oldest first
		if item.RequisitionID != want.ID || item.Number != want.Number {
			t.Fatalf("pending[%d] = %s/%s, want oldest-first %s/%s", i, item.Number, item.RequisitionID, want.Number, want.ID)
		}
		if item.StepNo != 1 || item.RoleRequired != "approver" || item.StepID == "" {
			t.Fatalf("pending[%d] current step = %+v, want step 1 of the materialized chain", i, item)
		}
		if item.Currency != "IDR" || item.TotalCents != 100 || item.SubmittedAt.IsZero() {
			t.Fatalf("pending[%d] header = %+v", i, item)
		}
	}

	// --- Spend: per vendor+currency over the list rows, windows applied ---
	pos := purchaseorder.NewStore(db)
	type key struct{ vendor, currency string }
	list30 := map[key]int64{}
	list90 := map[key]int64{}
	listTotal := 0
	for page := 1; ; page++ {
		filter, err := purchaseorder.ValidateList(purchaseorder.ListFilter{Page: int32(page), PageSize: 100})
		if err != nil {
			t.Fatalf("validate list: %v", err)
		}
		rows, total, err := pos.List(ctx, dashTenantA, filter)
		if err != nil {
			t.Fatalf("po list page %d: %v", page, err)
		}
		if page == 1 {
			listTotal = total
		}
		if len(rows) == 0 {
			break
		}
		for _, po := range rows {
			// The register basis: issued (not draft/cancelled, issued_at
			// set) AND inside the 90-day window — the same filter the
			// store's WHERE applies.
			if po.Status == "draft" || po.Status == "cancelled" || po.IssuedAt == nil ||
				po.IssuedAt.Before(now.Add(-SpendWindow90d)) {
				continue
			}
			k := key{po.VendorID, po.Currency}
			list90[k] += po.TotalCents
			if !po.IssuedAt.Before(now.Add(-SpendWindow30d)) {
				list30[k] += po.TotalCents
			}
		}
	}
	if len(summary.SpendByVendor) != len(list90) {
		t.Fatalf("spend rows = %d, want %d (one per vendor+currency with issued spend)", len(summary.SpendByVendor), len(list90))
	}
	for _, row := range summary.SpendByVendor {
		k := key{row.VendorID, row.Currency}
		if row.SpendCents30d != list30[k] || row.SpendCents90d != list90[k] {
			t.Fatalf("spend[%s %s] = (%d, %d), want list sums (%d, %d)",
				row.VendorName, row.Currency, row.SpendCents30d, row.SpendCents90d, list30[k], list90[k])
		}
		if row.VendorName == "" {
			t.Fatalf("spend row %+v carries no vendor name", row)
		}
	}
	// Spend totals per currency = the unbounded sums over the same rows.
	total30, total90 := map[string]int64{}, map[string]int64{}
	for k, cents := range list30 {
		total30[k.currency] += cents
	}
	for k, cents := range list90 {
		total90[k.currency] += cents
	}
	if len(summary.SpendTotals) != len(total90) {
		t.Fatalf("spend totals = %+v, want one per currency (%v)", summary.SpendTotals, total90)
	}
	for _, c := range summary.SpendTotals {
		if c.ThirtyDayCents != total30[c.Currency] || c.NinetyDayCents != total90[c.Currency] {
			t.Fatalf("totals[%s] = (%d, %d), want (%d, %d)",
				c.Currency, c.ThirtyDayCents, c.NinetyDayCents, total30[c.Currency], total90[c.Currency])
		}
	}

	// --- Distribution: zeros included, sum equals the unfiltered list total ---
	if len(summary.POStatusDistribution) != 7 {
		t.Fatalf("distribution = %d rows, want all 7 statuses", len(summary.POStatusDistribution))
	}
	distSum := int64(0)
	seen := map[string]int64{}
	for _, d := range summary.POStatusDistribution {
		seen[d.Status] = d.Count
		distSum += d.Count
	}
	if distSum != int64(listTotal) {
		t.Fatalf("distribution sum = %d, want list total %d", distSum, listTotal)
	}
	for _, status := range []string{"draft", "issued", "acknowledged", "received", "closed", "cancelled"} {
		if seen[status] != 1 {
			t.Fatalf("distribution[%s] = %d, want 1", status, seen[status])
		}
	}
	if seen["partially_received"] != 0 {
		t.Fatalf("distribution[partially_received] = %d, want the zero filler", seen["partially_received"])
	}
	// Descending order is by 90-day spend; vX IDR carries the largest sum.
	if summary.SpendByVendor[0].VendorID != vX.ID || summary.SpendByVendor[0].Currency != "IDR" {
		t.Fatalf("top spend row = %+v, want vX IDR (largest 90-day sum)", summary.SpendByVendor[0])
	}
}

// TestStoreSummaryCrossTenantDenial is the multi-tenancy denial case: tenant
// B's summary never sees tenant A's queue or spend — an empty snapshot, not
// a leak (RLS scopes every query of the one transaction).
func TestStoreSummaryCrossTenantDenial(t *testing.T) {
	db := openTestDB(t)
	cleanupDash(t, db)
	t.Cleanup(func() { cleanupDash(t, db) })
	ctx := context.Background()

	if _, err := approval.NewStore(db).ReplaceRules(ctx, dashTenantA, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	dashRequisition(t, db, dashTenantA, dashBasket) // pending step in tenant A

	summary, err := NewStore(db).Summary(ctx, dashTenantB, time.Now().UTC())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Pending.Total != 0 || len(summary.Pending.Items) != 0 ||
		len(summary.SpendByVendor) != 0 || len(summary.SpendTotals) != 0 {
		t.Fatalf("cross-tenant summary = %+v, want an empty snapshot", summary)
	}
	for _, d := range summary.POStatusDistribution {
		if d.Count != 0 {
			t.Fatalf("cross-tenant distribution[%s] = %d, want 0", d.Status, d.Count)
		}
	}
}

// TestStoreSummaryFailurePath is the store's failure path: the platform
// tenant context rejects an empty tenant id before any query runs.
func TestStoreSummaryFailurePath(t *testing.T) {
	db := openTestDB(t)
	if _, err := NewStore(db).Summary(context.Background(), "", time.Now().UTC()); err == nil {
		t.Fatal("summary with empty tenant id: want error, got nil")
	}
}

// TestStatusOrderCanonical pins the distribution's rendering order against
// the domain's status constants (the widget's bars stay stable).
func TestStatusOrderCanonical(t *testing.T) {
	want := []string{
		purchaseorder.StatusDraft, purchaseorder.StatusIssued,
		purchaseorder.StatusAcknowledged, purchaseorder.StatusPartiallyReceived,
		purchaseorder.StatusReceived, purchaseorder.StatusClosed,
		purchaseorder.StatusCancelled,
	}
	if len(POStatusOrder) != len(want) {
		t.Fatalf("POStatusOrder = %v, want %v", POStatusOrder, want)
	}
	for i := range want {
		if POStatusOrder[i] != want[i] {
			t.Fatalf("POStatusOrder[%d] = %s, want %s", i, POStatusOrder[i], want[i])
		}
	}
}
