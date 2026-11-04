package goodsreceipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	grnTenantA   = "66666666-0000-0000-0000-000000000001"
	grnTenantB   = "66666666-0000-0000-0000-000000000002"
	grnRequester = "66666666-0000-0000-0000-0000000000aa"
	grnApprover  = "66666666-0000-0000-0000-0000000000bb"
	grnBuyer     = "66666666-0000-0000-0000-0000000000cc"
)

// openTestDB mirrors the requisition/purchaseorder harness: apply
// migrations, open a pool, skip without TEST_DATABASE_URL.
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

// cleanupGRN wipes everything the goods-receipt tests create in the shared
// test tenants: outbox rows, goods receipts (items cascade), GRN counters,
// purchase orders (po_items cascade), PO counters, requisitions (items/
// steps cascade), PR counters, approval rules, and the test vendors.
func cleanupGRN(t *testing.T, db *pgxpool.Pool) {
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
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'GRN-37-%'`,
		} {
			if _, err := tx.Exec(ctx, stmt, grnTenantA, grnTenantB); err != nil {
				return err
			}
		}
		return nil
	})
}

// acknowledgedPO drives the real flow — rules, requisition, submit, approve,
// convert, issue, acknowledge — so every goods receipt lands on a PO the
// domain state machine actually walked here. The returned PO carries its
// items with ids (LoadForUpdate loads them; receipt lines reference them).
func acknowledgedPO(t *testing.T, db *pgxpool.Pool, tenantID string, basket []requisition.Item) purchaseorder.PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	if _, err := approval.NewStore(db).ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	v, err := vendor.NewStore(db).Create(ctx, tenantID, vendor.CreateInput{
		Code: "GRN-37-" + tenantID[len(tenantID)-1:], Name: "PT GRN Fixture", UserID: grnBuyer,
	})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: grnRequester, Currency: "IDR", Items: basket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, grnRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
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
		ActorID: grnApprover, Roles: []string{"approver"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	pos := purchaseorder.NewStore(db)
	po, _, err := pos.CreateFromRequisition(ctx, tenantID, r.ID, v.ID, grnBuyer)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, err := pos.Issue(ctx, tenantID, po.ID, grnBuyer); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ack, err := pos.Acknowledge(ctx, tenantID, po.ID, grnBuyer)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	for _, it := range ack.Items {
		if it.ID == "" {
			t.Fatalf("acknowledged PO items carry no ids: %+v", ack.Items)
		}
	}
	return ack
}

var grnBasket = []requisition.Item{
	{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
	{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
}

// grnOutboxRows loads the grn.posted rows of one PO — every successful post
// writes exactly one, failures none.
func grnOutboxRows(t *testing.T, db *pgxpool.Pool, poID string) []map[string]any {
	t.Helper()
	rows := []map[string]any{}
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		r, err := tx.Query(context.Background(), `
			SELECT event_type, entity_type, entity_id::text, version, payload, actor_id::text, trace_id
			FROM procurement.outbox
			WHERE entity_id = $1 AND event_type = 'grn.posted'`, poID)
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
		t.Fatalf("load grn.posted rows: %v", err)
	}
	return rows
}

// grnRowCount counts the tenant's goods_receipts rows under RLS bypass.
func grnRowCount(t *testing.T, db *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.goods_receipts WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count goods receipts: %v", err)
	}
	return n
}

// poAudit reads the purchase_orders audit columns the same-status branch
// must not move.
func poAudit(t *testing.T, db *pgxpool.Pool, poID string) (status string, updatedAt, updatedBy string) {
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

// itemQty reads one po_items row's qty_received.
func itemQty(t *testing.T, db *pgxpool.Pool, poItemID string) int32 {
	t.Helper()
	var qty int32
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT qty_received FROM procurement.po_items WHERE id = $1`, poItemID).Scan(&qty)
	}); err != nil {
		t.Fatalf("read item qty: %v", err)
	}
	return qty
}

// grnItemByQty picks the PO line with the given qty_ordered — po_items rows
// of one conversion share created_at, so load order is not the basket order
// (the dev-note 035 multiset note); tests must never key on the slice index.
func grnItemByQty(t *testing.T, po purchaseorder.PurchaseOrder, qty int32) purchaseorder.Item {
	t.Helper()
	for _, it := range po.Items {
		if it.Qty == qty {
			return it
		}
	}
	t.Fatalf("no PO line with qty_ordered %d in %+v", qty, po.Items)
	return purchaseorder.Item{}
}

// AC (#37 happy path, all three branches): posting drives the real flow —
// first partial acknowledged → partially_received, second partial
// same-status (no purchase_orders write at all), full completion →
// received — each post committing exactly one grn.posted outbox row whose
// entity is the purchase order and whose payload carries the pinned fields,
// with the actor and the context trace on the envelope.
func TestStorePostGoodsReceiptBranches(t *testing.T) {
	db := openTestDB(t)
	cleanupGRN(t, db)
	t.Cleanup(func() { cleanupGRN(t, db) })
	ctx := logger.WithTraceID(context.Background(), "trace-grn-37")
	s := NewStore(db)
	po := acknowledgedPO(t, db, grnTenantA, grnBasket)
	laptop, mouse := grnItemByQty(t, po, 2), grnItemByQty(t, po, 3)

	// Branch (a): first partial from acknowledged.
	grn1, after1, err := s.Post(ctx, grnTenantA, PostInput{
		POID:  po.ID,
		Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}},
		Notes: "first pallet",
		Actor: grnBuyer,
	})
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if grn1.Number != "GRN-000001" || grn1.ReceivedBy != grnBuyer || grn1.POID != po.ID {
		t.Fatalf("grn1 = %+v", grn1)
	}
	if after1.Status != purchaseorder.StatusPartiallyReceived || itemQty(t, db, laptop.ID) != 1 {
		t.Fatalf("after1 = %s, laptop qty = %d", after1.Status, itemQty(t, db, laptop.ID))
	}
	rows := grnOutboxRows(t, db, po.ID)
	if len(rows) != 1 {
		t.Fatalf("grn.posted rows after post 1 = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["entity_type"] != "purchase_order" || row["entity_id"] != po.ID ||
		row["actor_id"] != grnBuyer || row["trace_id"] != "trace-grn-37" || row["version"] != int32(1) {
		t.Fatalf("grn.posted row = %+v", row)
	}
	payload := row["payload"].(map[string]any)
	if payload["grn_id"] != grn1.ID || payload["number"] != "GRN-000001" ||
		payload["po_number"] != po.Number || payload["po_status"] != purchaseorder.StatusPartiallyReceived ||
		payload["received_by"] != grnBuyer {
		t.Fatalf("grn.posted payload = %+v", payload)
	}
	if items, ok := payload["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("payload items = %+v", payload["items"])
	} else if first, _ := items[0].(map[string]any); first["po_item_id"] != laptop.ID || first["qty_received"] != float64(1) {
		t.Fatalf("payload items = %+v", items)
	}

	// Branch (c): second partial, same status — no purchase_orders write at
	// all (the audit columns must not move), yet a full GRN + outbox row.
	beforeStatus, beforeUpdated, beforeBy := poAudit(t, db, po.ID)
	grn2, after2, err := s.Post(ctx, grnTenantA, PostInput{
		POID:  po.ID,
		Items: []Item{{POItemID: mouse.ID, QtyReceived: 2}},
		Actor: grnBuyer,
	})
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if grn2.Number != "GRN-000002" {
		t.Fatalf("grn2 number = %s, want GRN-000002", grn2.Number)
	}
	if after2.Status != purchaseorder.StatusPartiallyReceived || itemQty(t, db, mouse.ID) != 2 {
		t.Fatalf("after2 = %s, mouse qty = %d", after2.Status, itemQty(t, db, mouse.ID))
	}
	afterStatus, afterUpdated, afterBy := poAudit(t, db, po.ID)
	if afterStatus != beforeStatus || afterUpdated != beforeUpdated || afterBy != beforeBy {
		t.Fatalf("same-status branch moved purchase_orders: (%s,%s,%s) -> (%s,%s,%s)",
			beforeStatus, beforeUpdated, beforeBy, afterStatus, afterUpdated, afterBy)
	}
	if rows := grnOutboxRows(t, db, po.ID); len(rows) != 2 {
		t.Fatalf("grn.posted rows after post 2 = %d, want 2 (same-status branch writes one too)", len(rows))
	}

	// Branch (b): completion → received.
	_, after3, err := s.Post(ctx, grnTenantA, PostInput{
		POID: po.ID,
		Items: []Item{
			{POItemID: laptop.ID, QtyReceived: 1},
			{POItemID: mouse.ID, QtyReceived: 1},
		},
		Actor: grnBuyer,
	})
	if err != nil {
		t.Fatalf("post 3: %v", err)
	}
	if after3.Status != purchaseorder.StatusReceived ||
		itemQty(t, db, laptop.ID) != 2 || itemQty(t, db, mouse.ID) != 3 {
		t.Fatalf("after3 = %s, qtys = (%d, %d)", after3.Status, itemQty(t, db, laptop.ID), itemQty(t, db, mouse.ID))
	}
	if rows := grnOutboxRows(t, db, po.ID); len(rows) != 3 {
		t.Fatalf("grn.posted rows after post 3 = %d, want 3", len(rows))
	}
	if last := grnOutboxRows(t, db, po.ID)[2]["payload"].(map[string]any); last["po_status"] != purchaseorder.StatusReceived {
		t.Fatalf("post 3 payload po_status = %v, want received", last["po_status"])
	}
}

// AC (#37 failure paths): every guard rejects with nothing written — no GRN
// rows, no qty_received move, no outbox row, no burned number — and the
// over-receipt answer carries the pinned detail line verbatim.
func TestStorePostGoodsReceiptGuards(t *testing.T) {
	db := openTestDB(t)
	cleanupGRN(t, db)
	t.Cleanup(func() { cleanupGRN(t, db) })
	ctx := context.Background()
	s := NewStore(db)
	po := acknowledgedPO(t, db, grnTenantA, grnBasket)
	laptop := grnItemByQty(t, po, 2)

	post := func(in PostInput) error {
		_, _, err := s.Post(ctx, grnTenantA, in)
		return err
	}

	// Over-receipt: the pinned detail line, exact.
	err := post(PostInput{POID: po.ID, Items: []Item{{POItemID: laptop.ID, QtyReceived: 3}}, Actor: grnBuyer})
	want := fmt.Sprintf("grn_over_receipt: po_item=%s received=0 requested=3 ordered=2", laptop.ID)
	if err == nil || err.Error() != want {
		t.Fatalf("over-receipt err = %v, want %q", err, want)
	}
	if !errors.Is(err, purchaseorder.ErrOverReceipt) {
		t.Fatalf("errors.Is(err, ErrOverReceipt) = false")
	}
	if n := grnRowCount(t, db, grnTenantA); n != 0 {
		t.Fatalf("goods_receipts rows after rejection = %d, want 0", n)
	}
	if rows := grnOutboxRows(t, db, po.ID); len(rows) != 0 {
		t.Fatalf("grn.posted rows after rejection = %d, want 0", len(rows))
	}
	if qty := itemQty(t, db, laptop.ID); qty != 0 {
		t.Fatalf("laptop qty after rejection = %d, want 0", qty)
	}

	// Shape and reference failures, table-driven.
	unknown := "77777777-0000-0000-0000-0000000000ee"
	for name, tc := range map[string]struct {
		in      PostInput
		wantErr error
		wantMsg string
	}{
		"empty items":    {PostInput{POID: po.ID, Actor: grnBuyer}, ErrItemsEmpty, "grn_items_empty"},
		"zero qty":       {PostInput{POID: po.ID, Items: []Item{{POItemID: laptop.ID}}, Actor: grnBuyer}, ErrQtyInvalid, "grn_qty_invalid"},
		"duplicate line": {PostInput{POID: po.ID, Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}, {POItemID: laptop.ID, QtyReceived: 1}}, Actor: grnBuyer}, ErrPOItemDuplicate, "grn_po_item_duplicate"},
		"unknown line":   {PostInput{POID: po.ID, Items: []Item{{POItemID: unknown, QtyReceived: 1}}, Actor: grnBuyer}, purchaseorder.ErrReceiptItemUnknown, "grn_po_item_unknown"},
		"notes over bound": {PostInput{POID: po.ID, Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}},
			Notes: string(make([]byte, MaxNotesLen+1)), Actor: grnBuyer}, ErrNotesInvalid, "grn_notes_invalid"},
		"bad po id":  {PostInput{POID: "nope", Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}}, Actor: grnBuyer}, purchaseorder.ErrIDInvalid, "po_id_invalid"},
		"unknown po": {PostInput{POID: unknown, Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}}, Actor: grnBuyer}, purchaseorder.ErrNotFound, "po_not_found"},
	} {
		if err := post(tc.in); !errors.Is(err, tc.wantErr) || err.Error() != tc.wantMsg {
			t.Errorf("%s: err = (%v, %q), want (%v, %q)", name, err, errMsg(err), tc.wantErr, tc.wantMsg)
		}
	}

	// Illegal status: the draft PO a conversion just created cannot receive.
	draft := draftPO(t, db, grnTenantA)
	err = post(PostInput{POID: draft.ID, Items: []Item{{POItemID: draft.Items[0].ID, QtyReceived: 1}}, Actor: grnBuyer})
	if !errors.Is(err, purchaseorder.ErrIllegalTransition) || err.Error() != "po_illegal_transition" {
		t.Fatalf("draft receipt err = %v, want po_illegal_transition", err)
	}

	// Nothing was written by any of the guards: no rows anywhere.
	if n := grnRowCount(t, db, grnTenantA); n != 0 {
		t.Fatalf("goods_receipts rows after all guards = %d, want 0", n)
	}
	// And no number was burned: the first successful post is GRN-000001.
	grn, _, err := s.Post(ctx, grnTenantA, PostInput{
		POID: po.ID, Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}}, Actor: grnBuyer,
	})
	if err != nil || grn.Number != "GRN-000001" {
		t.Fatalf("first success after failures = (%s, %v), want GRN-000001", grn.Number, err)
	}

	// Cross-tenant denial: tenant B cannot even see tenant A's PO.
	_, _, err = s.Post(ctx, grnTenantB, PostInput{
		POID: po.ID, Items: []Item{{POItemID: laptop.ID, QtyReceived: 1}}, Actor: grnBuyer,
	})
	if !errors.Is(err, purchaseorder.ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

// draftPO converts a fresh approved requisition and stops at draft.
func draftPO(t *testing.T, db *pgxpool.Pool, tenantID string) purchaseorder.PurchaseOrder {
	t.Helper()
	ctx := context.Background()
	basket := []requisition.Item{{Description: "Keyboard", Qty: 5, UnitPriceCents: 400000, Category: "it"}}
	r := approvedDraftSource(t, db, tenantID, basket)
	v, err := vendor.NewStore(db).Create(ctx, tenantID, vendor.CreateInput{
		Code: "GRN-37-D" + tenantID[len(tenantID)-1:], Name: "PT GRN Draft", UserID: grnBuyer,
	})
	if err != nil {
		t.Fatalf("create draft vendor: %v", err)
	}
	po, _, err := purchaseorder.NewStore(db).CreateFromRequisition(ctx, tenantID, r.ID, v.ID, grnBuyer)
	if err != nil {
		t.Fatalf("convert draft source: %v", err)
	}
	// The conversion response carries no item ids; load them via a tenant
	// read (Issue is not called — the PO stays draft).
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

// approvedDraftSource drives rules → requisition → submit → approve.
func approvedDraftSource(t *testing.T, db *pgxpool.Pool, tenantID string, basket []requisition.Item) requisition.Requisition {
	t.Helper()
	ctx := context.Background()
	if _, err := approval.NewStore(db).ReplaceRules(ctx, tenantID, []approval.Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	reqs := requisition.NewStore(db)
	apps := approval.NewStore(db)
	r, err := reqs.Create(ctx, tenantID, requisition.CreateInput{
		RequesterID: grnRequester, Currency: "IDR", Items: basket,
	})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if _, err := reqs.Submit(ctx, tenantID, r.ID, grnRequester, "", func(tx pgx.Tx, req requisition.Requisition) error {
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
		ActorID: grnApprover, Roles: []string{"approver"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	return r
}

// errMsg renders nil errors as "<nil>" for table messages.
func errMsg(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// AC (#37 concurrency): two simultaneous receipts fighting over the last
// units of one line serialize on the PO row lock — exactly one commits, the
// loser gets the clean 409 over-receipt answer — while two partials that
// fit both commit as separate GRNs. Numbers stay gap-free per tenant and
// independent across tenants.
func TestStoreGoodsReceiptConcurrencyAndNumbering(t *testing.T) {
	db := openTestDB(t)
	cleanupGRN(t, db)
	t.Cleanup(func() { cleanupGRN(t, db) })
	ctx := context.Background()
	s := NewStore(db)

	// One line of 10 units: two racers of 6 each — only one fits.
	basket := []requisition.Item{{Description: "Monitor", Qty: 10, UnitPriceCents: 2500000, Category: "it"}}
	po := acknowledgedPO(t, db, grnTenantA, basket)
	line := po.Items[0]

	var wg sync.WaitGroup
	var wins, losses atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Post(ctx, grnTenantA, PostInput{
				POID: po.ID, Items: []Item{{POItemID: line.ID, QtyReceived: 6}}, Actor: grnBuyer,
			})
			if err == nil {
				wins.Add(1)
				return
			}
			if errors.Is(err, purchaseorder.ErrOverReceipt) {
				losses.Add(1)
				return
			}
			t.Errorf("racer err = %v, want a win or the over-receipt answer", err)
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 1 {
		t.Fatalf("last-unit race = (%d wins, %d losses), want (1, 1)", wins.Load(), losses.Load())
	}
	if qty := itemQty(t, db, line.ID); qty != 6 {
		t.Fatalf("line qty after race = %d, want 6", qty)
	}
	if n := grnRowCount(t, db, grnTenantA); n != 1 {
		t.Fatalf("goods_receipts rows after race = %d, want 1", n)
	}
	if rows := grnOutboxRows(t, db, po.ID); len(rows) != 1 {
		t.Fatalf("grn.posted rows after race = %d, want 1", len(rows))
	}

	// Two further partials that fit (2 + 2 <= 4 remaining) both commit as
	// separate GRNs — run in parallel this time: they serialize on the PO
	// row lock, the second sees the first's advance, and the completing one
	// moves the PO to received (the same-status branch under concurrency).
	var partialWG sync.WaitGroup
	var partialWins atomic.Int32
	for i := 0; i < 2; i++ {
		partialWG.Add(1)
		go func() {
			defer partialWG.Done()
			if _, _, err := s.Post(ctx, grnTenantA, PostInput{
				POID: po.ID, Items: []Item{{POItemID: line.ID, QtyReceived: 2}}, Actor: grnBuyer,
			}); err != nil {
				t.Errorf("parallel fitting partial: %v", err)
				return
			}
			partialWins.Add(1)
		}()
	}
	partialWG.Wait()
	if partialWins.Load() != 2 {
		t.Fatalf("fitting partials committed = %d, want 2", partialWins.Load())
	}
	if qty := itemQty(t, db, line.ID); qty != 10 {
		t.Fatalf("line qty after partials = %d, want 10", qty)
	}
	if n := grnRowCount(t, db, grnTenantA); n != 3 {
		t.Fatalf("goods_receipts rows after partials = %d, want 3", n)
	}
	if rows := grnOutboxRows(t, db, po.ID); len(rows) != 3 {
		t.Fatalf("grn.posted rows after partials = %d, want 3", len(rows))
	}
	if status, _, _ := poAudit(t, db, po.ID); status != purchaseorder.StatusReceived {
		t.Fatalf("po status after full run = %s, want received", status)
	}

	// Tenant independence: tenant B's first GRN is its own GRN-000001.
	poB := acknowledgedPO(t, db, grnTenantB, basket)
	grnB, _, err := s.Post(ctx, grnTenantB, PostInput{
		POID: poB.ID, Items: []Item{{POItemID: poB.Items[0].ID, QtyReceived: 1}}, Actor: grnBuyer,
	})
	if err != nil || grnB.Number != "GRN-000001" {
		t.Fatalf("tenant B first grn = (%s, %v), want GRN-000001", grnB.Number, err)
	}
}
