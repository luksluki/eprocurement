package main

// Integration coverage for the issue #110 live writers against a real
// PostgreSQL: the dataset the generator builds must satisfy the live schema
// constraints, the domain invariants must hold as SQL over the written rows,
// and row-level security must scope every tenant-carrying table (plus
// identity.users) to exactly one tenant under the app role.
//
// Skipped without TEST_DATABASE_URL, the dashboard store_test.go pattern —
// the Makefile's TEST_DATABASE_URL passthrough from .env supplies the
// compose database.

import (
	"context"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io/fs"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	identitymigrations "github.com/luksluki/eprocurement/services/identity/migrations"
	procurementmigrations "github.com/luksluki/eprocurement/services/procurement/migrations"
)

// openLiveTestDB applies the identity + procurement migrations (idempotent
// against an already-migrated stack) and returns a pool, skipping without
// TEST_DATABASE_URL.
func openLiveTestDB(t *testing.T) *pgxpool.Pool {
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
	for _, m := range []struct {
		schema string
		fs     fs.FS
	}{
		{"identity", identitymigrations.FS()},
		{"procurement", procurementmigrations.FS()},
	} {
		if _, err := dbmigrate.Apply(context.Background(), migPool, m.schema, m.fs); err != nil {
			migPool.Close()
			t.Fatalf("apply %s migrations: %v", m.schema, err)
		}
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// TestLiveWritersSchemaAndInvariants writes a small generated dataset into
// the live schemas and checks the database-level invariants: totals equal
// their line sums, decided steps carry a role-holding non-requester actor,
// PO qty_received equals the goods-receipt item sums, matched invoices sit
// on GRN-bearing invoicable POs, and no counter/match_settings rows were
// written (the lazy-init policy).
func TestLiveWritersSchemaAndInvariants(t *testing.T) {
	db := openLiveTestDB(t)
	ctx := context.Background()

	cfg := config{tenants: 2, vendorsPer: 5, requisitions: 60, events: 0, seed: 777}
	ds := buildDataset(cfg, rand.New(rand.NewSource(cfg.seed)), time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))

	roleIDs, err := loadRoleIDs(ctx, db)
	if err != nil {
		t.Fatalf("load identity roles: %v", err)
	}
	tenantIDs := make([]string, len(ds.tenants))
	for i, tt := range ds.tenants {
		tenantIDs[i] = tt.id
	}
	t.Cleanup(func() {
		if err := deleteLivePostgres(ctx, db, tenantIDs); err != nil {
			t.Fatalf("cleanup live rows: %v", err)
		}
	})
	writeLivePostgres(ctx, db, ds, roleIDs)

	// The writer must not create counter or match_settings rows: number
	// continuation is the stores' lazy init from max(number), and match
	// tolerances default to 0%/0% while no row exists (issue #110 policy).
	for _, table := range []string{
		`SELECT count(*) FROM procurement.requisition_counters WHERE tenant_id = ANY($1::uuid[])`,
		`SELECT count(*) FROM procurement.po_counters WHERE tenant_id = ANY($1::uuid[])`,
		`SELECT count(*) FROM procurement.grn_counters WHERE tenant_id = ANY($1::uuid[])`,
		`SELECT count(*) FROM procurement.match_settings WHERE tenant_id = ANY($1::uuid[])`,
	} {
		assertCount(t, db, table, tenantIDs, 0, "counter/match_settings rows")
	}

	// (a) totals = Σ(qty × unit price) on all three document kinds.
	assertCount(t, db, `
		SELECT count(*) FROM procurement.requisitions r
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON r.tenant_id = t.tid
		WHERE r.total_cents <> (SELECT coalesce(sum(i.qty * i.unit_price_cents), 0)
		                        FROM procurement.requisition_items i WHERE i.requisition_id = r.id)`,
		tenantIDs, 0, "requisition totals ≠ Σ lines")
	assertCount(t, db, `
		SELECT count(*) FROM procurement.purchase_orders p
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON p.tenant_id = t.tid
		WHERE p.total_cents <> (SELECT coalesce(sum(i.qty_ordered * i.unit_price_cents), 0)
		                        FROM procurement.po_items i WHERE i.po_id = p.id)`,
		tenantIDs, 0, "PO totals ≠ Σ lines")
	assertCount(t, db, `
		SELECT count(*) FROM procurement.invoices v
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON v.tenant_id = t.tid
		WHERE v.total_cents <> (SELECT coalesce(sum(l.qty * l.unit_price_cents), 0)
		                        FROM procurement.invoice_lines l WHERE l.invoice_id = v.id)`,
		tenantIDs, 0, "invoice totals ≠ Σ lines")

	// (b) approval steps consistent with the requisition state machine.
	assertCount(t, db, `
		SELECT count(*) FROM procurement.requisitions r
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON r.tenant_id = t.tid
		WHERE (r.status IN ('approved','closed')
		       AND EXISTS (SELECT 1 FROM procurement.approval_steps s
		                   WHERE s.requisition_id = r.id AND s.status <> 'approved'))
		   OR (r.status = 'submitted'
		       AND EXISTS (SELECT 1 FROM procurement.approval_steps s
		                   WHERE s.requisition_id = r.id AND s.status <> 'pending'))
		   OR (r.status = 'rejected'
		       AND (NOT EXISTS (SELECT 1 FROM procurement.approval_steps s
		                        WHERE s.requisition_id = r.id AND s.step_no = 1 AND s.status = 'rejected')
		            OR EXISTS (SELECT 1 FROM procurement.approval_steps s
		                       WHERE s.requisition_id = r.id AND s.step_no > 1 AND s.status <> 'skipped')))`,
		tenantIDs, 0, "approval steps inconsistent with requisition status")

	// (c) decided steps: another seeded user holding role_required, never
	// the requester (self-approval ban, docs/rbac.md:37).
	assertCount(t, db, `
		SELECT count(*) FROM procurement.approval_steps s
		JOIN procurement.requisitions r ON r.id = s.requisition_id
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON r.tenant_id = t.tid
		WHERE s.status IN ('approved','rejected')
		  AND (s.decided_by IS NULL
		       OR s.decided_by = r.requester_id
		       OR s.decided_by NOT IN (
		           SELECT u.id FROM identity.users u
		           JOIN identity.user_roles ur ON ur.user_id = u.id
		           JOIN identity.roles ro ON ro.id = ur.role_id
		           WHERE u.tenant_id = r.tenant_id AND ro.code = s.role_required))`,
		tenantIDs, 0, "decided step actor violates the role/self-approval rules")

	// (d) po_items.qty_received = Σ goods_receipt_items.
	assertCount(t, db, `
		SELECT count(*) FROM procurement.po_items i
		JOIN procurement.purchase_orders p ON p.id = i.po_id
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON p.tenant_id = t.tid
		WHERE i.qty_received <> (SELECT coalesce(sum(g.qty_received), 0)
		                         FROM procurement.goods_receipt_items g
		                         WHERE g.po_item_id = i.id)`,
		tenantIDs, 0, "po_items.qty_received ≠ Σ goods_receipt_items")

	// (e) matched invoices only on GRN-bearing invoicable POs.
	assertCount(t, db, `
		SELECT count(*) FROM procurement.invoices v
		JOIN procurement.purchase_orders p ON p.id = v.po_id
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON v.tenant_id = t.tid
		WHERE v.status = 'matched'
		  AND (p.status NOT IN ('acknowledged','partially_received','received')
		       OR NOT EXISTS (SELECT 1 FROM procurement.goods_receipts g WHERE g.po_id = p.id))`,
		tenantIDs, 0, "matched invoice on a non-invoicable or GRN-less PO")

	// (f) goods_receipts.received_by references a seeded user of the tenant.
	assertCount(t, db, `
		SELECT count(*) FROM procurement.goods_receipts g
		JOIN (SELECT unnest($1::uuid[]) AS tid) t ON g.tenant_id = t.tid
		WHERE g.received_by IS NULL
		   OR g.received_by NOT IN (SELECT id FROM identity.users WHERE tenant_id = g.tenant_id)`,
		tenantIDs, 0, "goods receipt received_by is not a seeded user of the tenant")

	// Reconciliation against the demo export happens in the dev note's
	// `make seed` evidence; here the live counts must match the dataset.
	for tenantIdx, tenant := range ds.tenants {
		var got int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM procurement.requisitions WHERE tenant_id = $1`, tenant.id).Scan(&got); err != nil {
			t.Fatalf("count requisitions of tenant %s: %v", tenant.id, err)
		}
		if want := countRequisitions(ds, tenantIdx); got != want {
			t.Fatalf("tenant %s requisitions: got %d, want %d", tenant.id, got, want)
		}
	}

	// The reset path must remove every live row of exactly these tenants.
	if err := deleteLivePostgres(ctx, db, tenantIDs); err != nil {
		t.Fatalf("delete live rows: %v", err)
	}
	assertCount(t, db, `
		SELECT (SELECT count(*) FROM procurement.requisitions WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM procurement.purchase_orders WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM procurement.goods_receipts WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM procurement.invoices WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM procurement.vendors WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM procurement.approval_rules WHERE tenant_id = ANY($1::uuid[]))
		     + (SELECT count(*) FROM identity.users WHERE tenant_id = ANY($1::uuid[]))`,
		tenantIDs, 0, "live rows survived deleteLivePostgres")
}

// TestLiveRowsTenantIsolation is the cross-tenant denial case: under
// SET ROLE eproc_app + app.tenant_id = A, every tenant-carrying
// procurement.* table and identity.users expose exactly tenant A's rows —
// tenant B's rows are invisible (the RLS policies the migrations define).
func TestLiveRowsTenantIsolation(t *testing.T) {
	db := openLiveTestDB(t)
	ctx := context.Background()

	cfg := config{tenants: 2, vendorsPer: 4, requisitions: 40, events: 0, seed: 778}
	ds := buildDataset(cfg, rand.New(rand.NewSource(cfg.seed)), time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))

	roleIDs, err := loadRoleIDs(ctx, db)
	if err != nil {
		t.Fatalf("load identity roles: %v", err)
	}
	tenantIDs := make([]string, len(ds.tenants))
	for i, tt := range ds.tenants {
		tenantIDs[i] = tt.id
	}
	t.Cleanup(func() {
		if err := deleteLivePostgres(ctx, db, tenantIDs); err != nil {
			t.Fatalf("cleanup live rows: %v", err)
		}
	})
	writeLivePostgres(ctx, db, ds, roleIDs)

	a, b := ds.tenants[0], ds.tenants[1]
	if countRequisitions(ds, 0) == 0 || countRequisitions(ds, 1) == 0 {
		t.Fatal("test dataset must populate both tenants")
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE eproc_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, a.id); err != nil {
		t.Fatalf("set tenant: %v", err)
	}

	// Directly tenant-carrying tables + identity.users: the scoped count
	// equals tenant A's full count and tenant B's rows are all invisible.
	scoped := map[string]string{
		"procurement.vendors":              `SELECT count(*) FROM procurement.vendors`,
		"procurement.approval_rules":       `SELECT count(*) FROM procurement.approval_rules`,
		"procurement.requisitions":         `SELECT count(*) FROM procurement.requisitions`,
		"procurement.purchase_orders":      `SELECT count(*) FROM procurement.purchase_orders`,
		"procurement.goods_receipts":       `SELECT count(*) FROM procurement.goods_receipts`,
		"procurement.invoices":             `SELECT count(*) FROM procurement.invoices`,
		"procurement.outbox":               `SELECT count(*) FROM procurement.outbox`,
		"procurement.decision_idempotency": `SELECT count(*) FROM procurement.decision_idempotency`,
		"procurement.requisition_counters": `SELECT count(*) FROM procurement.requisition_counters`,
		"procurement.po_counters":          `SELECT count(*) FROM procurement.po_counters`,
		"procurement.grn_counters":         `SELECT count(*) FROM procurement.grn_counters`,
		"procurement.match_settings":       `SELECT count(*) FROM procurement.match_settings`,
		"identity.users":                   `SELECT count(*) FROM identity.users`,
	}
	want := map[string]int{
		"procurement.vendors":              cfg.vendorsPer,
		"procurement.approval_rules":       2,
		"procurement.requisitions":         countRequisitions(ds, 0),
		"procurement.purchase_orders":      countPOs(ds, 0),
		"procurement.goods_receipts":       countGRNs(ds, 0),
		"procurement.invoices":             countInvoices(ds, 0),
		"procurement.outbox":               0,
		"procurement.decision_idempotency": 0,
		"procurement.requisition_counters": 0,
		"procurement.po_counters":          0,
		"procurement.grn_counters":         0,
		"procurement.match_settings":       0,
		"identity.users":                   len(seedRoles),
	}
	for table, sql := range scoped {
		var got int
		if err := tx.QueryRow(ctx, sql).Scan(&got); err != nil {
			t.Fatalf("scoped count %s: %v", table, err)
		}
		if got != want[table] {
			t.Fatalf("%s under app.tenant_id=A: got %d rows, want %d (tenant B leaked or A lost rows)", table, got, want[table])
		}
	}

	// Child tables derive tenancy from their parents: under A's scope only
	// A's requisition items and approval steps are visible.
	var itemsA, itemsB int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE r.tenant_id::text = $1),
		       count(*) FILTER (WHERE r.tenant_id::text = $2)
		FROM procurement.requisition_items i
		JOIN procurement.requisitions r ON r.id = i.requisition_id`,
		a.id, b.id).Scan(&itemsA, &itemsB); err != nil {
		t.Fatalf("scoped requisition_items: %v", err)
	}
	if itemsB != 0 {
		t.Fatalf("requisition_items of tenant B visible under A's scope: %d", itemsB)
	}
	var stepsB int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM procurement.approval_steps s
		JOIN procurement.requisitions r ON r.id = s.requisition_id
		WHERE r.tenant_id::text = $1`, b.id).Scan(&stepsB); err != nil {
		t.Fatalf("scoped approval_steps: %v", err)
	}
	if stepsB != 0 {
		t.Fatalf("approval_steps of tenant B visible under A's scope: %d", stepsB)
	}
}

func countRequisitions(ds dataset, tenantIdx int) int {
	return countTenant(ds.tenants[tenantIdx].id, len(ds.requisitions), func(i int) string { return ds.requisitions[i].tenantID })
}

func countPOs(ds dataset, tenantIdx int) int {
	return countTenant(ds.tenants[tenantIdx].id, len(ds.purchaseOrders), func(i int) string { return ds.purchaseOrders[i].tenantID })
}

func countGRNs(ds dataset, tenantIdx int) int {
	return countTenant(ds.tenants[tenantIdx].id, len(ds.grns), func(i int) string { return ds.grns[i].tenantID })
}

func countInvoices(ds dataset, tenantIdx int) int {
	return countTenant(ds.tenants[tenantIdx].id, len(ds.invoices), func(i int) string { return ds.invoices[i].tenantID })
}

func countTenant(tenantID string, n int, idAt func(int) string) int {
	c := 0
	for i := 0; i < n; i++ {
		if idAt(i) == tenantID {
			c++
		}
	}
	return c
}

func assertCount(t *testing.T, db *pgxpool.Pool, sql string, args []string, want int, label string) {
	t.Helper()
	var got int
	if err := db.QueryRow(context.Background(), sql, args).Scan(&got); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", label, got, want)
	}
}
