package purchaseorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
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
			`DELETE FROM procurement.goods_receipts WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM procurement.grn_counters WHERE tenant_id IN ($1, $2)`,
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

// poStatus reads the stored status under RLS bypass.
func poStatus(t *testing.T, db *pgxpool.Pool, id string) string {
	t.Helper()
	var status string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM procurement.purchase_orders WHERE id = $1`, id).Scan(&status)
	}); err != nil {
		t.Fatalf("read po status: %v", err)
	}
	return status
}

// forcePOStatus flips a PO straight to a post-acknowledgement status and
// bumps one line's qty_received — the #37 stand-in (goods receipts advance
// qty_received and drive the receipt edges; #36 only needs the state to
// exist to test close/cancel). WithReceipts=false leaves qty_received at 0,
// which cannot happen past acknowledgement in production but isolates the
// edge guard from the receipt guard here.
func forcePOStatus(t *testing.T, db *pgxpool.Pool, id, status string, withReceipts bool) {
	t.Helper()
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`UPDATE procurement.purchase_orders SET status = $2 WHERE id = $1`, id, status); err != nil {
			return err
		}
		if withReceipts {
			_, err := tx.Exec(ctx, `
				UPDATE procurement.po_items SET qty_received = 1
				WHERE po_id = $1 AND id = (
					SELECT id FROM procurement.po_items WHERE po_id = $1
					ORDER BY created_at, id LIMIT 1)`, id)
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("force po %s: %v", status, err)
	}
}

// poLifecycleRows loads the outbox rows of one lifecycle event type for a PO
// — transitions must write exactly one each, failures none.
func poLifecycleRows(t *testing.T, db *pgxpool.Pool, poID, eventType string) []map[string]any {
	t.Helper()
	rows := []map[string]any{}
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		r, err := tx.Query(context.Background(), `
			SELECT entity_type, entity_id::text, version, payload, actor_id::text, trace_id
			FROM procurement.outbox
			WHERE entity_id = $1 AND event_type = $2`, poID, eventType)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var entityType, entityID, actorID, traceID string
			var version int32
			var payload []byte
			if err := r.Scan(&entityType, &entityID, &version, &payload, &actorID, &traceID); err != nil {
				return err
			}
			var p map[string]any
			if err := json.Unmarshal(payload, &p); err != nil {
				return err
			}
			rows = append(rows, map[string]any{
				"entity_type": entityType, "entity_id": entityID, "version": version,
				"payload": p, "actor_id": actorID, "trace_id": traceID,
			})
		}
		return r.Err()
	}); err != nil {
		t.Fatalf("load %s rows: %v", eventType, err)
	}
	return rows
}

// draftPO converts a fresh approved requisition into the draft PO every
// lifecycle test starts from. The vendor code carries a process-unique
// suffix so parallel drafts never collide on the tenant's unique code.
var poVendorSeq int64

func draftPO(t *testing.T, db *pgxpool.Pool, tenantID string) PurchaseOrder {
	t.Helper()
	req := sqlApproved(t, db, tenantID)
	seq := atomic.AddInt64(&poVendorSeq, 1)
	v := poVendor(t, db, tenantID, fmt.Sprintf("L%d", seq))
	po, _, err := NewStore(db).CreateFromRequisition(context.Background(), tenantID, req.ID, v.ID, poBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return po
}

// AC: each legal store transition moves the status, stamps issued_at on
// issue, and commits exactly one matching outbox row with the pinned
// transition payload — issue → acknowledge → close (short-close from
// partially_received, normal close from received).
func TestStoreLifecycleHappyPath(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	// Short-close arm: acknowledge, partial receipt, close.
	short := draftPO(t, db, poTenantA)
	issued, err := s.Issue(ctx, poTenantA, short.ID, poBuyer)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Status != StatusIssued || issued.IssuedAt == nil || issued.IssuedAt.IsZero() {
		t.Fatalf("issue result = %s/%v, want issued/<stamped>", issued.Status, issued.IssuedAt)
	}
	rows := poLifecycleRows(t, db, short.ID, OutboxIssued)
	if len(rows) != 1 {
		t.Fatalf("po.issued rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["entity_type"] != "purchase_order" || row["entity_id"] != short.ID ||
		row["version"] != int32(1) || row["actor_id"] != poBuyer {
		t.Fatalf("po.issued row = %+v", row)
	}
	payload := row["payload"].(map[string]any)
	want := map[string]any{
		"number": short.Number, "requisition_id": short.RequisitionID,
		"vendor_id": short.VendorID, "status": StatusIssued,
		"total_cents": float64(short.TotalCents), "currency": "IDR", "performed_by": poBuyer,
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("po.issued payload[%s] = %v, want %v", k, payload[k], v)
		}
	}
	if len(payload) != len(want) {
		t.Errorf("po.issued payload has extra fields: %+v", payload)
	}

	acknowledged, err := s.Acknowledge(ctx, poTenantA, short.ID, poBuyer)
	if err != nil || acknowledged.Status != StatusAcknowledged {
		t.Fatalf("acknowledge = (%s, %v), want acknowledged", acknowledged.Status, err)
	}
	if got := len(poLifecycleRows(t, db, short.ID, OutboxAcknowledged)); got != 1 {
		t.Fatalf("po.acknowledged rows = %d, want 1", got)
	}

	forcePOStatus(t, db, short.ID, StatusPartiallyReceived, true)
	closed, err := s.Close(ctx, poTenantA, short.ID, poBuyer)
	if err != nil || closed.Status != StatusClosed {
		t.Fatalf("short-close = (%s, %v), want closed", closed.Status, err)
	}
	if got := len(poLifecycleRows(t, db, short.ID, OutboxClosed)); got != 1 {
		t.Fatalf("po.closed rows = %d, want 1", got)
	}
	if got := poStatus(t, db, short.ID); got != StatusClosed {
		t.Fatalf("stored status = %s, want closed", got)
	}

	// Normal-close arm: acknowledge, full receipt, close.
	full := draftPO(t, db, poTenantA)
	if _, err := s.Issue(ctx, poTenantA, full.ID, poBuyer); err != nil {
		t.Fatalf("issue full: %v", err)
	}
	if _, err := s.Acknowledge(ctx, poTenantA, full.ID, poBuyer); err != nil {
		t.Fatalf("acknowledge full: %v", err)
	}
	forcePOStatus(t, db, full.ID, StatusReceived, true)
	if _, err := s.Close(ctx, poTenantA, full.ID, poBuyer); err != nil {
		t.Fatalf("close full: %v", err)
	}
	if got := poStatus(t, db, full.ID); got != StatusClosed {
		t.Fatalf("stored status = %s, want closed", got)
	}

	// Cancel arm: draft → cancelled commits po.cancelled.
	direct := draftPO(t, db, poTenantA)
	cancelled, err := s.Cancel(ctx, poTenantA, direct.ID, poBuyer)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("cancel = (%s, %v), want cancelled", cancelled.Status, err)
	}
	if got := len(poLifecycleRows(t, db, direct.ID, OutboxCancelled)); got != 1 {
		t.Fatalf("po.cancelled rows = %d, want 1", got)
	}
	// issued → cancelled as well.
	mid := draftPO(t, db, poTenantA)
	if _, err := s.Issue(ctx, poTenantA, mid.ID, poBuyer); err != nil {
		t.Fatalf("issue mid: %v", err)
	}
	if _, err := s.Cancel(ctx, poTenantA, mid.ID, poBuyer); err != nil {
		t.Fatalf("cancel mid: %v", err)
	}
	if got := len(poLifecycleRows(t, db, mid.ID, OutboxCancelled)); got != 1 {
		t.Fatalf("po.cancelled rows (from issued) = %d, want 1", got)
	}
}

// AC (guards + rollback): an illegal transition fails with the sentinel,
// leaves the stored status untouched, and writes no outbox row — the whole
// transaction rolls back. Cancel after a receipt is ErrHasReceipts, also with
// no write. Cross-tenant ids are indistinguishable from missing; bad ids
// fail fast.
func TestStoreLifecycleGuards(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	// Each case gets a fresh draft PO so the cases stay order-independent
	// (the cancel case consumes its PO's one legal cancel).
	cases := []struct {
		name    string
		call    func(id string) error
		want    string // stored status after the case
		wantRow string // event type whose rows must stay 0 (the failing write)
	}{
		{"acknowledge draft", func(id string) error {
			_, err := s.Acknowledge(ctx, poTenantA, id, poBuyer)
			return err
		}, StatusDraft, OutboxAcknowledged},
		{"close draft", func(id string) error {
			_, err := s.Close(ctx, poTenantA, id, poBuyer)
			return err
		}, StatusDraft, OutboxClosed},
		{"cancel then reissue", func(id string) error { // cancelled is terminal
			if _, err := s.Cancel(ctx, poTenantA, id, poBuyer); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			_, err := s.Issue(ctx, poTenantA, id, poBuyer)
			return err
		}, StatusCancelled, OutboxIssued},
	}
	for _, tc := range cases {
		po := draftPO(t, db, poTenantA)
		if err := tc.call(po.ID); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s: err = %v, want ErrIllegalTransition", tc.name, err)
		}
		if got := poStatus(t, db, po.ID); got != tc.want {
			t.Fatalf("%s: stored status = %s, want %s", tc.name, got, tc.want)
		}
		if got := len(poLifecycleRows(t, db, po.ID, tc.wantRow)); got != 0 {
			t.Fatalf("%s: %s rows = %d, want 0 (full rollback)", tc.name, tc.wantRow, got)
		}
	}

	// Cancel after a receipt: the PO is pinned — po_has_receipts, no write.
	pinned := draftPO(t, db, poTenantA)
	if _, err := s.Issue(ctx, poTenantA, pinned.ID, poBuyer); err != nil {
		t.Fatalf("issue pinned: %v", err)
	}
	if _, err := s.Acknowledge(ctx, poTenantA, pinned.ID, poBuyer); err != nil {
		t.Fatalf("acknowledge pinned: %v", err)
	}
	forcePOStatus(t, db, pinned.ID, StatusAcknowledged, true)
	if _, err := s.Cancel(ctx, poTenantA, pinned.ID, poBuyer); !errors.Is(err, ErrHasReceipts) {
		t.Fatalf("cancel with receipts: err = %v, want ErrHasReceipts", err)
	}
	if got := poStatus(t, db, pinned.ID); got != StatusAcknowledged {
		t.Fatalf("status after pinned cancel = %s, want acknowledged", got)
	}
	if got := len(poLifecycleRows(t, db, pinned.ID, OutboxCancelled)); got != 0 {
		t.Fatalf("pinned cancel rows = %d, want 0", got)
	}

	// Multi-tenancy: tenant B cannot see tenant A's PO (RLS → 404); a
	// non-UUID id fails fast.
	if _, err := s.Issue(ctx, poTenantB, pinned.ID, poBuyer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant issue: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Issue(ctx, poTenantA, "not-a-uuid", poBuyer); !errors.Is(err, ErrIDInvalid) {
		t.Fatalf("bad id: err = %v, want ErrIDInvalid", err)
	}
}

// AC (serialization): two concurrent issues of one draft PO serialize on the
// row lock — exactly one wins, the loser gets ErrIllegalTransition, and
// exactly one po.issued row commits.
func TestStoreIssueConcurrency(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	ctx := context.Background()

	po := draftPO(t, db, poTenantA)
	const racers = 2
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Issue(ctx, poTenantA, po.ID, poBuyer)
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrIllegalTransition):
			// the loser
		default:
			t.Fatalf("racer %d: err = %v, want success or ErrIllegalTransition", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if got := poStatus(t, db, po.ID); got != StatusIssued {
		t.Fatalf("stored status = %s, want issued", got)
	}
	if got := len(poLifecycleRows(t, db, po.ID, OutboxIssued)); got != 1 {
		t.Fatalf("po.issued rows = %d, want 1", got)
	}
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

// seedListPOs creates six POs in tenant A (four drafts, one issued, one
// acknowledged) and one in tenant B (never visible to tenant A), via the
// real conversion path; every list test calls cleanupPO first so the
// per-tenant numbering is deterministic (PO-000001..6 for tenant A).
func seedListPOs(t *testing.T, db *pgxpool.Pool, s *Store) []PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	var created []PurchaseOrder
	for i := 0; i < 6; i++ {
		created = append(created, draftPO(t, db, poTenantA))
	}
	// The oldest becomes issued, the second-oldest acknowledged — the status
	// filter shapes need non-draft values.
	issued, err := s.Issue(ctx, poTenantA, created[0].ID, poBuyer)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	created[0] = issued
	ack, err := s.Issue(ctx, poTenantA, created[1].ID, poBuyer)
	if err != nil {
		t.Fatalf("issue 2: %v", err)
	}
	created[1], err = s.Acknowledge(ctx, poTenantA, ack.ID, poBuyer)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	// Another tenant's row: never visible to tenant A listings.
	draftPO(t, db, poTenantB)
	return created
}

func listPOOK(t *testing.T, s *Store, f ListFilter) ([]PurchaseOrder, int) {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	rows, total, err := s.List(t.Context(), poTenantA, filter)
	if err != nil {
		t.Fatalf("List(%+v): %v", f, err)
	}
	return rows, total
}

// AC (#38): the list filters combine (status equality, number prefix with
// literal wildcards, both orderings), stays tenant-scoped, orders by number
// descending, and returns header-only rows.
func TestStoreListFilters(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	created := seedListPOs(t, db, s)

	// No filter: all six of tenant A's rows, newest number first.
	rows, total := listPOOK(t, s, ListFilter{PageSize: 10})
	if total != 6 || len(rows) != 6 {
		t.Fatalf("unfiltered = %d rows (total %d), want 6", len(rows), total)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Number <= rows[i].Number {
			t.Fatalf("not descending: %s then %s", rows[i-1].Number, rows[i].Number)
		}
	}
	// Header-only: the list never loads items even for POs that have them.
	for _, po := range rows {
		if len(po.Items) != 0 {
			t.Fatalf("list row carries items: %+v", po)
		}
		if po.VendorID == "" || po.RequisitionID == "" || po.TotalCents == 0 {
			t.Fatalf("header fields missing: %+v", po)
		}
	}

	// Status filter: only the issued row (created[0] is PO-000001).
	rows, total = listPOOK(t, s, ListFilter{Status: StatusIssued, PageSize: 10})
	if total != 1 || rows[0].ID != created[0].ID || rows[0].IssuedAt == nil {
		t.Fatalf("issued = %+v (total %d)", rows, total)
	}

	// Query: number prefix, case-insensitive input — matches exactly one.
	rows, total = listPOOK(t, s, ListFilter{Query: created[2].Number, PageSize: 10})
	if total != 1 || rows[0].ID != created[2].ID {
		t.Fatalf("full number = %v (total %d)", rows, total)
	}
	// The common prefix ("PO-0000" at least) matches all six.
	rows, total = listPOOK(t, s, ListFilter{Query: "po-0000", PageSize: 10})
	if total != 6 {
		t.Fatalf("prefix = %v (total %d)", rows, total)
	}
	// % is literal, not a wildcard (also exercises the past-the-end COUNT
	// fallback with zero matches).
	rows, total = listPOOK(t, s, ListFilter{Query: "%", PageSize: 10})
	if total != 0 || len(rows) != 0 {
		t.Fatalf("literal %% = %v (total %d)", rows, total)
	}

	// Combined: acknowledged + the common prefix → exactly created[1].
	rows, total = listPOOK(t, s, ListFilter{Query: "PO-0000", Status: StatusAcknowledged, PageSize: 10})
	if total != 1 || rows[0].ID != created[1].ID {
		t.Fatalf("combined acknowledged = %v (total %d)", rows, total)
	}

	// Cross-tenant: tenant A's rows never appear in tenant B's listing, and
	// tenant B sees exactly its own single row.
	rowsB, totalB, err := s.List(t.Context(), poTenantB, mustValidatePO(t, ListFilter{Query: "PO-"}))
	if err != nil {
		t.Fatalf("tenantB list: %v", err)
	}
	if totalB != 1 || len(rowsB) != 1 || rowsB[0].TenantID != poTenantB {
		t.Fatalf("tenantB sees %d rows (total %d), want its single row", len(rowsB), totalB)
	}
}

// Pagination: deep pages land correctly, a page past the end is empty but
// still reports the true total (the COUNT fallback), and zero values apply
// the defaults (page 1, page size 20).
func TestStoreListPagination(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	created := seedListPOs(t, db, s)

	// Descending order: PO-000006 (newest) first. Page 2 with size 2 = the
	// third- and fourth-newest rows.
	rows, total := listPOOK(t, s, ListFilter{Page: 2, PageSize: 2})
	if total != 6 || len(rows) != 2 ||
		rows[0].ID != created[3].ID || rows[1].ID != created[2].ID {
		t.Fatalf("page 2 = %v (total %d)", rows, total)
	}

	// Page beyond the end: empty page, total still reported.
	rows, total = listPOOK(t, s, ListFilter{Page: 5, PageSize: 2})
	if total != 6 || len(rows) != 0 {
		t.Fatalf("overflow page = %d rows (total %d)", len(rows), total)
	}

	// Defaults land on page 1 with page size 20 (the newest row first).
	rows, _ = listPOOK(t, s, ListFilter{})
	if len(rows) != 6 || rows[0].ID != created[5].ID {
		t.Fatalf("defaults = %v", rows)
	}
}

// AC (#38): Get returns the full aggregate with item ids; cross-tenant ids
// are indistinguishable from missing (ErrNotFound, the RLS answer) and
// non-UUID ids fail fast.
func TestStoreGet(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	cleanupPO(t, db)
	t.Cleanup(func() { cleanupPO(t, db) })
	created := seedListPOs(t, db, s)

	got, err := s.Get(context.Background(), poTenantA, created[1].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created[1].ID || got.Status != StatusAcknowledged || got.Number != created[1].Number {
		t.Fatalf("get = %+v", got)
	}
	// The detail read owns the bodies: items load with their ids (receipt
	// lines reference po_item ids) and copied quantities.
	if len(got.Items) != len(poBasket) {
		t.Fatalf("items not loaded: %+v", got.Items)
	}
	qtyByDesc := map[string]Item{}
	for _, it := range got.Items {
		if it.ID == "" {
			t.Fatalf("item id missing: %+v", it)
		}
		qtyByDesc[it.Description] = it
	}
	if qtyByDesc["Laptop"].Qty != 2 || qtyByDesc["Mouse"].Qty != 3 {
		t.Fatalf("items not copied verbatim: %+v", got.Items)
	}

	// Cross-tenant read is indistinguishable from missing.
	if _, err := s.Get(context.Background(), poTenantB, created[1].ID); !errors.Is(err, ErrNotFound) || err != ErrNotFound {
		t.Fatalf("cross-tenant err = %v, want ErrNotFound", err)
	}
	// Non-UUID id fails fast.
	if _, err := s.Get(context.Background(), poTenantA, "not-a-uuid"); !errors.Is(err, ErrIDInvalid) || err != ErrIDInvalid {
		t.Fatalf("bad id err = %v, want ErrIDInvalid", err)
	}
}

func mustValidatePO(t *testing.T, f ListFilter) ListFilter {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	return filter
}
