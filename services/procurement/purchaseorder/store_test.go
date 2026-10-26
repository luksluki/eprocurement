package purchaseorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	poTenantA   = "55555555-0000-0000-0000-000000000001"
	poTenantB   = "55555555-0000-0000-0000-000000000002"
	poRequester = "55555555-0000-0000-0000-0000000000aa"
	poApprover  = "55555555-0000-0000-0000-0000000000bb"
	poBuyer     = "55555555-0000-0000-0000-0000000000cc"
)

// openTestDB mirrors the requisition harness: apply migrations, open a pool,
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

// cleanupPO wipes everything the PO tests create in the shared test tenants:
// outbox rows, purchase orders (items cascade), PO counters, requisitions
// (items/steps cascade), PR counters, approval rules, and the test vendors.
func cleanupPO(t *testing.T, db *pgxpool.Pool) {
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
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'PO-35-%'`,
		} {
			if _, err := tx.Exec(ctx, stmt, poTenantA, poTenantB); err != nil {
				return err
			}
		}
		return nil
	})
}

var poBasket = []requisition.Item{
	{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
	{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
}

// poVendor creates a test vendor of tenantID (code PO-35-<n>).
func poVendor(t *testing.T, db *pgxpool.Pool, tenantID, n string) vendor.Vendor {
	t.Helper()
	v, err := vendor.NewStore(db).Create(context.Background(), tenantID, vendor.CreateInput{
		Code: "PO-35-" + n, Name: "PT Vendor " + n, UserID: poBuyer,
	})
	if err != nil {
		t.Fatalf("create vendor %s: %v", n, err)
	}
	return v
}

// approvedRequisition drives the real flow — create, submit (with rule
// materialization), and a one-step approve — so the happy path converts a
// requisition the approval engine actually approved.
func approvedRequisition(t *testing.T, db *pgxpool.Pool, tenantID string) requisition.Requisition {
	t.Helper()
	ctx := context.Background()
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	if _, err := apps.ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: poRequester, Currency: "IDR", Items: poBasket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, poRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
		_, err := apps.MaterializeTx(ctx, tx, req)
		return err
	}); err != nil {
		t.Fatalf("submit requisition: %v", err)
	}
	steps, err := apps.ListSteps(ctx, tenantID, r.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("list steps = (%+v, %v), want one", steps, err)
	}
	final, _, err := apps.Decide(ctx, tenantID, approval.DecideInput{
		StepID: steps[0].ID, Decision: approval.DecisionApprove,
		ActorID: poApprover, Roles: []string{"approver"},
	})
	if err != nil || final.Status != requisition.StatusApproved {
		t.Fatalf("decide = (%s, %v), want approved", final.Status, err)
	}
	final.Items = poBasket
	return final
}

// sqlApproved creates a requisition with items and flips it straight to
// approved — the lighter fixture for gate/numbering tests whose focus is the
// conversion, not the approval engine (the requisition state machine itself
// is covered by the requisition package).
func sqlApproved(t *testing.T, db *pgxpool.Pool, tenantID string) requisition.Requisition {
	t.Helper()
	ctx := context.Background()
	r, err := requisition.NewStore(db).Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: poRequester, Currency: "IDR", Items: poBasket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE procurement.requisitions SET status = 'approved' WHERE id = $1`, r.ID)
		return err
	}); err != nil {
		t.Fatalf("force approved: %v", err)
	}
	r.Status = requisition.StatusApproved
	r.Items = poBasket
	return r
}

// sqlDraft creates a plain draft requisition with items — the negative
// fixture for the status gate.
func sqlDraft(t *testing.T, db *pgxpool.Pool, tenantID string) requisition.Requisition {
	t.Helper()
	r, err := requisition.NewStore(db).Create(context.Background(), tenantID, requisition.CreateInput{
		RequesterID: poRequester, Currency: "IDR", Items: poBasket,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return r
}

// requisitionStatus reads the stored status under RLS bypass.
func requisitionStatus(t *testing.T, db *pgxpool.Pool, id string) string {
	t.Helper()
	var status string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM procurement.requisitions WHERE id = $1`, id).Scan(&status)
	}); err != nil {
		t.Fatalf("read requisition status: %v", err)
	}
	return status
}

// poOutboxRows counts procurement.outbox po.created rows for one entity —
// replays must not grow the count. (The approval decide writes pr.* rows
// whose entity is the requisition, so scoping by event_type keeps the
// blocked-conversion assertions below exact.)
func poOutboxRows(t *testing.T, db *pgxpool.Pool, poID string) []map[string]any {
	t.Helper()
	rows := []map[string]any{}
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		r, err := tx.Query(context.Background(), `
			SELECT event_type, entity_type, entity_id::text, version, payload, actor_id::text, trace_id
			FROM procurement.outbox
			WHERE entity_id = $1 AND event_type = 'po.created'`, poID)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var eventType, entityType, entityID, actorID, traceID string
			var version int32
			var payload []byte
			if err := r.Scan(&eventType, &entityType, &entityID, &version, &payload, &actorID, &traceID); err != nil {
				return err
			}
			var p map[string]any
			if err := json.Unmarshal(payload, &p); err != nil {
				return err
			}
			rows = append(rows, map[string]any{
				"event_type": eventType, "entity_type": entityType, "entity_id": entityID,
				"version": version, "payload": p, "actor_id": actorID, "trace_id": traceID,
			})
		}
		return r.Err()
	}); err != nil {
		t.Fatalf("load outbox rows: %v", err)
	}
	return rows
}

// poNum reads the numeric part of a PO-%06d number (-1 when unparseable).
func poNum(number string) int {
	var v int
	if _, err := fmt.Sscanf(number, "PO-%d", &v); err != nil {
		return -1
	}
	return v
}

// AC: converting an approved requisition creates a draft PO — lines copied
// exactly once, total recomputed server-side (= the requisition's), currency
// copied, PO-%06d number — closes the requisition (approved → closed), and
// commits exactly one po.created outbox row with the pinned payload. The
// replay (same or different vendor_id, vendor since deactivated) returns the
// original PO untouched: no second write, no second outbox row.
func TestStoreConvertApprovedRequisition(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	req := approvedRequisition(t, db, poTenantA)
	active := poVendor(t, db, poTenantA, "A")
	other := poVendor(t, db, poTenantA, "B")

	po, created, err := s.CreateFromRequisition(ctx, poTenantA, req.ID, active.ID, poBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !created {
		t.Fatal("first conversion reports created=false")
	}
	if po.Number != "PO-000001" || po.Status != StatusDraft || po.IssuedAt != nil {
		t.Fatalf("po = %s/%s/%v, want PO-000001/draft/<nil>", po.Number, po.Status, po.IssuedAt)
	}
	if po.RequisitionID != req.ID || po.VendorID != active.ID || po.TenantID != poTenantA {
		t.Fatalf("po linkage = %+v", po)
	}
	if po.Currency != req.Currency || po.TotalCents != req.TotalCents || po.TotalCents != 15450000 {
		t.Fatalf("currency/total = %s/%d, want %s/%d", po.Currency, po.TotalCents, req.Currency, req.TotalCents)
	}
	// Items are compared as a multiset: po_items rows inside one conversion
	// share created_at, so their load order is not the requisition's line
	// order.
	if len(po.Items) != 2 {
		t.Fatalf("items not copied verbatim: %+v", po.Items)
	}
	lines := map[string]bool{}
	for _, it := range po.Items {
		if it.QtyReceived != 0 {
			t.Fatalf("qty_received = %d on creation, want 0: %+v", it.QtyReceived, po.Items)
		}
		lines[fmt.Sprintf("%s/%d/%d", it.Description, it.Qty, it.UnitPriceCents)] = true
	}
	if !lines["Laptop/2/7500000"] || !lines["Mouse/3/150000"] || len(lines) != 2 {
		t.Fatalf("items not copied verbatim: %v", lines)
	}
	if po.CreatedBy != poBuyer || po.UpdatedBy != poBuyer {
		t.Fatalf("audit = %s/%s, want %s", po.CreatedBy, po.UpdatedBy, poBuyer)
	}

	// The requisition closed through the conversion.
	if got := requisitionStatus(t, db, req.ID); got != requisition.StatusClosed {
		t.Fatalf("requisition status = %s, want closed", got)
	}

	// Exactly one outbox row: po.created, entity = the PO, pinned payload.
	rows := poOutboxRows(t, db, po.ID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["event_type"] != OutboxCreated || row["entity_type"] != "purchase_order" ||
		row["entity_id"] != po.ID || row["version"] != int32(1) || row["actor_id"] != poBuyer {
		t.Fatalf("outbox row = %+v", row)
	}
	payload := row["payload"].(map[string]any)
	want := map[string]any{
		"number": po.Number, "requisition_id": req.ID, "requisition_number": req.Number,
		"vendor_id": active.ID, "total_cents": float64(15450000), "currency": "IDR",
		"created_by": poBuyer,
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, payload[k], v)
		}
	}
	if len(payload) != len(want) {
		t.Errorf("payload has extra fields: %+v", payload)
	}

	// Replay with a DIFFERENT vendor_id: the original PO, byte for byte.
	replayed, replayCreated, err := s.CreateFromRequisition(ctx, poTenantA, req.ID, other.ID, poBuyer)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayCreated {
		t.Fatal("replay reports created=true")
	}
	if replayed.ID != po.ID || replayed.Number != po.Number || replayed.VendorID != active.ID ||
		!replayed.CreatedAt.Equal(po.CreatedAt) || len(replayed.Items) != len(po.Items) {
		t.Fatalf("replay = %+v, want the original PO %+v", replayed, po)
	}
	if got := len(poOutboxRows(t, db, po.ID)); got != 1 {
		t.Fatalf("outbox rows after replay = %d, want 1", got)
	}

	// Replay after the vendor was deactivated: the gate runs on the creation
	// path only — the original PO still answers.
	if _, err := vendor.NewStore(db).Deactivate(ctx, poTenantA, active.ID, poBuyer); err != nil {
		t.Fatalf("deactivate vendor: %v", err)
	}
	again, againCreated, err := s.CreateFromRequisition(ctx, poTenantA, req.ID, active.ID, poBuyer)
	if err != nil || again.ID != po.ID || again.VendorID != active.ID {
		t.Fatalf("replay after deactivate = (%+v, %v), want the original PO", again, err)
	}
	if againCreated {
		t.Fatal("replay after deactivate reports created=true")
	}
	if got := len(poOutboxRows(t, db, po.ID)); got != 1 {
		t.Fatalf("outbox rows after deactivated replay = %d, want 1", got)
	}
}

// AC (gates + rollback): the vendor gate and the status gate run inside the
// conversion transaction — a failure rolls the whole thing back (requisition
// keeps its status, no PO, no outbox row) and burns no number.
func TestStoreConvertGates(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	active := poVendor(t, db, poTenantA, "A")
	dead := poVendor(t, db, poTenantA, "D")
	otherTenant := poVendor(t, db, poTenantB, "T")
	if _, err := vendor.NewStore(db).Deactivate(ctx, poTenantA, dead.ID, poBuyer); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	draft := sqlDraft(t, db, poTenantA)
	blockedTarget := sqlApproved(t, db, poTenantA)

	// Deactivated vendor: FailedPrecondition sentinel, full rollback.
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, blockedTarget.ID, dead.ID, poBuyer); !errors.Is(err, vendor.ErrDeactivated) {
		t.Fatalf("deactivated vendor err = %v, want vendor.ErrDeactivated", err)
	}
	if got := requisitionStatus(t, db, blockedTarget.ID); got != requisition.StatusApproved {
		t.Fatalf("requisition after blocked convert = %s, want still approved", got)
	}
	if rows := poOutboxRows(t, db, blockedTarget.ID); len(rows) != 0 {
		t.Fatalf("outbox rows for blocked convert = %d, want 0", len(rows))
	}
	var poCount int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM procurement.purchase_orders WHERE requisition_id = $1`,
			blockedTarget.ID).Scan(&poCount)
	}); err != nil || poCount != 0 {
		t.Fatalf("PO rows after blocked convert = (%d, %v), want 0", poCount, err)
	}

	// The blocked conversion burned no number: the next conversion is 1.
	first, _, err := s.CreateFromRequisition(ctx, poTenantA, blockedTarget.ID, active.ID, poBuyer)
	if err != nil || first.Number != "PO-000001" {
		t.Fatalf("first conversion = (%s, %v), want PO-000001", first.Number, err)
	}

	// Missing and cross-tenant vendors are indistinguishable (404) — both
	// against the still-unconverted `second` (the gate only runs on the
	// creation path; blockedTarget already has its PO and would replay).
	second := sqlApproved(t, db, poTenantA)
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, second.ID,
		"55555555-0000-0000-0000-0000000000ff", poBuyer); !errors.Is(err, vendor.ErrNotFound) {
		t.Fatalf("missing vendor err = %v, want vendor.ErrNotFound", err)
	}
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, second.ID, otherTenant.ID, poBuyer); !errors.Is(err, vendor.ErrNotFound) {
		t.Fatalf("cross-tenant vendor err = %v, want vendor.ErrNotFound", err)
	}

	// Status gate: draft and (still) submitted requisitions do not convert.
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, draft.ID, active.ID, poBuyer); !errors.Is(err, ErrRequisitionNotApproved) {
		t.Fatalf("draft err = %v, want ErrRequisitionNotApproved", err)
	}
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE procurement.requisitions SET status = 'submitted' WHERE id = $1`, draft.ID)
		return err
	}); err != nil {
		t.Fatalf("force submitted: %v", err)
	}
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, draft.ID, active.ID, poBuyer); !errors.Is(err, ErrRequisitionNotApproved) {
		t.Fatalf("submitted err = %v, want ErrRequisitionNotApproved", err)
	}

	// AC (multi-tenancy): a cross-tenant requisition id is indistinguishable
	// from a missing one; bad ids fail fast before the transaction opens.
	if _, _, err := s.CreateFromRequisition(ctx, poTenantB, second.ID, otherTenant.ID, poBuyer); !errors.Is(err, requisition.ErrNotFound) {
		t.Fatalf("cross-tenant requisition err = %v, want requisition.ErrNotFound", err)
	}
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, "not-a-uuid", active.ID, poBuyer); !errors.Is(err, requisition.ErrIDInvalid) {
		t.Fatalf("bad requisition id err = %v, want requisition.ErrIDInvalid", err)
	}
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, second.ID, "nope", poBuyer); !errors.Is(err, ErrVendorIDInvalid) {
		t.Fatalf("bad vendor id err = %v, want ErrVendorIDInvalid", err)
	}
	if _, _, err := s.CreateFromRequisition(ctx, poTenantA, second.ID, "  ", poBuyer); !errors.Is(err, ErrVendorIDInvalid) {
		t.Fatalf("empty vendor id err = %v, want ErrVendorIDInvalid", err)
	}

	// The failed attempts after PO-000001 burned nothing: 2 follows.
	third := sqlApproved(t, db, poTenantA)
	next, _, err := s.CreateFromRequisition(ctx, poTenantA, third.ID, active.ID, poBuyer)
	if err != nil || next.Number != "PO-000002" {
		t.Fatalf("next conversion = (%s, %v), want PO-000002", next.Number, err)
	}
}

// AC (gap-free numbering + concurrency): sequential conversions number
// PO-000001..N; concurrent conversions of one requisition collapse into one
// PO and one outbox row (the loser replays the winner's PO); concurrent
// conversions of different requisitions produce one consecutive run; the
// two tenants' counters are independent.
func TestStoreNumberingAndConcurrency(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	active := poVendor(t, db, poTenantA, "A")
	otherTenant := poVendor(t, db, poTenantB, "T")

	// Sequential: four conversions, PO-000001..4.
	reqs := make([]requisition.Requisition, 0, 4)
	for i := 0; i < 4; i++ {
		reqs = append(reqs, sqlApproved(t, db, poTenantA))
	}
	for i, r := range reqs {
		po, _, err := s.CreateFromRequisition(ctx, poTenantA, r.ID, active.ID, poBuyer)
		if err != nil {
			t.Fatalf("convert %d: %v", i, err)
		}
		if want := fmt.Sprintf("PO-%06d", i+1); po.Number != want {
			t.Fatalf("convert %d number = %s, want %s", i, po.Number, want)
		}
	}

	// Concurrent conversions of ONE requisition: both goroutines succeed —
	// one creates, the other replays — with the identical PO, one row, one
	// po.created event.
	raced := sqlApproved(t, db, poTenantA)
	const racers = 2
	results := make([]PurchaseOrder, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			po, created, err := s.CreateFromRequisition(ctx, poTenantA, raced.ID, active.ID, poBuyer)
			results[i], errs[i] = po, err
			_ = created // both racers succeed: one creator, one replay
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v (want one winner + one replay)", i, err)
		}
	}
	if results[0].ID != results[1].ID || results[0].Number != results[1].Number ||
		!results[0].CreatedAt.Equal(results[1].CreatedAt) {
		t.Fatalf("racers diverged: %+v vs %+v", results[0], results[1])
	}
	var rows int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM procurement.purchase_orders WHERE requisition_id = $1`,
			raced.ID).Scan(&rows)
	}); err != nil || rows != 1 {
		t.Fatalf("PO rows for raced requisition = (%d, %v), want 1", rows, err)
	}
	if got := len(poOutboxRows(t, db, results[0].ID)); got != 1 {
		t.Fatalf("outbox rows for raced conversion = %d, want 1", got)
	}

	// Concurrent conversions of DIFFERENT requisitions: one consecutive run
	// (the counter row lock serializes the allocators).
	const n = 4
	more := make([]requisition.Requisition, n)
	for i := range more {
		more[i] = sqlApproved(t, db, poTenantA)
	}
	nums := make([]string, n)
	cerrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			po, _, err := s.CreateFromRequisition(ctx, poTenantA, more[i].ID, active.ID, poBuyer)
			nums[i], cerrs[i] = po.Number, err
		}(i)
	}
	wg.Wait()
	seen := map[int]bool{}
	min, max := 1<<30, 0
	for i, err := range cerrs {
		if err != nil {
			t.Fatalf("concurrent convert %d: %v", i, err)
		}
		v := poNum(nums[i])
		if v <= 0 || seen[v] {
			t.Fatalf("bad/duplicate number %q", nums[i])
		}
		seen[v] = true
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if len(seen) != n || max-min+1 != n {
		t.Fatalf("numbers not one consecutive run: %v", nums)
	}

	// The tenants' counters are independent.
	indep := sqlApproved(t, db, poTenantB)
	poB, _, err := s.CreateFromRequisition(ctx, poTenantB, indep.ID, otherTenant.ID, poBuyer)
	if err != nil || poB.Number != "PO-000001" {
		t.Fatalf("tenant B first = (%s, %v), want PO-000001", poB.Number, err)
	}

	// Store round-trip of the copied lines: the replay lookup reloads the
	// stored header and items, which must equal what was created (order
	// compared as a multiset — po_items rows share one created_at inside
	// the conversion transaction, exactly like requisition_items).
	sortPO := func(items []Item) []string {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, fmt.Sprintf("%s/%d/%d", it.Description, it.Qty, it.UnitPriceCents))
		}
		sort.Strings(out)
		return out
	}
	roundTripped, _, err := s.CreateFromRequisition(ctx, poTenantA, reqs[0].ID, active.ID, poBuyer)
	if err != nil {
		t.Fatalf("round-trip replay: %v", err)
	}
	wantLines := sortPO([]Item{
		{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000},
		{Description: "Mouse", Qty: 3, UnitPriceCents: 150000},
	})
	if got := sortPO(roundTripped.Items); fmt.Sprintf("%v", got) != fmt.Sprintf("%v", wantLines) {
		t.Fatalf("round-trip items = %v, want %v", got, wantLines)
	}
	if roundTripped.TotalCents != 15450000 || roundTripped.Currency != "IDR" || roundTripped.Status != StatusDraft {
		t.Fatalf("round-trip header = %d/%s/%s", roundTripped.TotalCents, roundTripped.Currency, roundTripped.Status)
	}
}
