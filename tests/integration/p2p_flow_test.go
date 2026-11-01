package integration

// The procure-to-pay happy path (#58, PRD §8 core loop / §22 demo script):
// approval rules → vendor → PR(items) → submit → self-approval rejected →
// approve by a second user → convert to PO (idempotent replay asserted
// IN-flow: created=false, identical id) → issue → acknowledge → full GRN →
// invoice → match "matched" — every mutation through the in-process gRPC
// API, every event through outbox → publisher loop → RabbitMQ → consumer
// runner → MongoDB. The final asserts pin the FULL projected event set (an
// ordered sequence, tie-aware) and the closed audit-action set (only the
// five decision events).

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
)

// flowItems is the requisition basket: one line, 2 × 7,500,000 IDR.
func flowItems() []*procurementv1.RequisitionItem {
	return []*procurementv1.RequisitionItem{
		{Description: "P2P laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
	}
}

// happyEventSequence is the FULL event set of the story in business order.
// pr.step_approved and pr.approved share one occurred_at (one Decide call)
// and are matched tie-aware; po.created rides the conversion (#35 extends
// the §14.6 catalog), everything else is catalog.
var happyEventSequence = []string{
	"pr.submitted",
	"pr.step_approved",
	"pr.approved",
	"po.created",
	"po.issued",
	"po.acknowledged",
	"grn.posted",
	"invoice.matched",
}

// happyAuditActions is the closed audit set of the story: AuditAction maps
// exactly the decision events — pr.submitted and po.acknowledged are NOT
// decisions and must leave no audit row (documents/history/audit.go).
var happyAuditActions = []string{"approve", "approve", "issue", "post", "match"}

func TestProcureToPayHappyPath(t *testing.T) {
	e := newP2P(t)
	admin := rbac.RoleAdmin

	// --- install the approval chain (one-step IDR, approver role) ---------
	if _, err := e.client.ReplaceApprovalRules(e.as(e.requester, admin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: rbac.RoleApprover, ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("install approval rules: %v", err)
	}

	// --- vendor -----------------------------------------------------------
	v, err := e.client.CreateVendor(e.as(e.requester, admin),
		&procurementv1.CreateVendorRequest{
			Code: "P2P-58", Name: "PT Procure To Pay",
			ContactEmail: "sales@p2p.example", TaxId: "01.111.111.1-011.111",
		})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}

	// --- PR with items, then submit ---------------------------------------
	req, err := e.client.CreateRequisition(e.as(e.requester, admin),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: flowItems()})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	if req.GetTotalCents() != 15000000 {
		t.Fatalf("requisition total = %d, want 15000000", req.GetTotalCents())
	}

	step := time.Now()
	submitted, err := e.client.SubmitRequisition(e.as(e.requester, admin),
		&procurementv1.SubmitRequisitionRequest{Id: req.GetId(), IdempotencyKey: "p2p-submit-1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitted.GetStatus() != "submitted" || len(submitted.GetApprovalSteps()) != 1 {
		t.Fatalf("submitted = (%s, %d steps), want submitted with 1 step",
			submitted.GetStatus(), len(submitted.GetApprovalSteps()))
	}
	stepID := submitted.GetApprovalSteps()[0].GetId()
	if stepID == "" {
		t.Fatal("materialized step carries no id")
	}
	e.awaitProjected("submit", 1, step)

	// --- self-approval is forbidden, admin or not (#30 rule) --------------
	if _, err := e.client.DecideApproval(e.as(e.requester, admin),
		&procurementv1.DecideApprovalRequest{StepId: stepID, Decision: "approve"}); status.Code(err) != codes.PermissionDenied ||
		status.Convert(err).Message() != "approval_self_forbidden" {
		t.Fatalf("self approve: got (%v, %q), want PermissionDenied approval_self_forbidden",
			status.Code(err), status.Convert(err).Message())
	}
	// The rejection changed nothing: step pending, requisition submitted.
	after := func() *procurementv1.Requisition {
		r, err := e.client.GetRequisition(e.as(e.requester, admin),
			&procurementv1.GetRequisitionRequest{Id: req.GetId()})
		if err != nil {
			t.Fatalf("get requisition: %v", err)
		}
		return r
	}()
	if after.GetStatus() != "submitted" || after.GetApprovalSteps()[0].GetStatus() != "pending" {
		t.Fatalf("after rejected self-approve = (%s, %s), want (submitted, pending)",
			after.GetStatus(), after.GetApprovalSteps()[0].GetStatus())
	}

	// --- approve as the second user: step + terminal events ---------------
	step = time.Now()
	decided, err := e.client.DecideApproval(e.as(e.approver, admin),
		&procurementv1.DecideApprovalRequest{
			StepId: stepID, Decision: "approve", Reason: "p2p ok", IdempotencyKey: "p2p-decide-1",
		})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decided.GetRequisition().GetStatus() != "approved" {
		t.Fatalf("requisition after approve = %s, want approved", decided.GetRequisition().GetStatus())
	}
	e.awaitProjected("approve", 3, step) // submitted + step_approved + approved

	// --- convert to PO, then replay the conversion (idempotency AC) -------
	step = time.Now()
	wrapped, err := e.client.CreatePurchaseOrder(e.as(e.requester, admin),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: req.GetId(), VendorId: v.GetId()})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !wrapped.GetCreated() {
		t.Fatal("first convert: created = false, want true")
	}
	po := wrapped.GetPurchaseOrder()
	if po.GetStatus() != "draft" || po.GetTotalCents() != 15000000 || po.GetNumber() != "PO-000001" {
		t.Fatalf("converted PO = (%s, %d, %s), want (draft, 15000000, PO-000001)",
			po.GetStatus(), po.GetTotalCents(), po.GetNumber())
	}
	if len(po.GetItems()) != 1 || po.GetItems()[0].GetQtyOrdered() != 2 {
		t.Fatalf("PO items = %+v, want the copied single line qty 2", po.GetItems())
	}
	e.awaitProjected("convert", 4, step) // + po.created

	// Replay: the same requisition returns the EXISTING PO with
	// created=false and the identical id — "cannot convert twice" is a
	// replay, not an error (the unique index backs it). A different
	// vendor_id proves the vendor gate is skipped on replay.
	replay, err := e.client.CreatePurchaseOrder(e.as(e.requester, admin),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: req.GetId(), VendorId: "dddddddd-0000-0000-0000-00000000dead"})
	if err != nil {
		t.Fatalf("replay convert: %v", err)
	}
	if replay.GetCreated() {
		t.Fatal("replay convert: created = true, want false")
	}
	if replay.GetPurchaseOrder().GetId() != po.GetId() ||
		replay.GetPurchaseOrder().GetCreatedAt() != po.GetCreatedAt() ||
		replay.GetPurchaseOrder().GetVendorId() != v.GetId() {
		t.Fatalf("replay PO = (%s, %s), want the untouched original (%s, %s)",
			replay.GetPurchaseOrder().GetId(), replay.GetPurchaseOrder().GetCreatedAt(),
			po.GetId(), po.GetCreatedAt())
	}
	if got := e.poTotal(); got != 1 {
		t.Fatalf("purchase orders after replay = %d, want 1", got)
	}
	if got := e.outboxTotal(); got != 4 {
		t.Fatalf("outbox rows after replay = %d, want 4 (the replay wrote nothing)", got)
	}

	// --- issue → acknowledge ----------------------------------------------
	step = time.Now()
	issued, err := e.client.IssuePurchaseOrder(e.as(e.requester, admin),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.GetStatus() != "issued" || issued.GetIssuedAt() == "" {
		t.Fatalf("issued PO = (%s, issued_at %q)", issued.GetStatus(), issued.GetIssuedAt())
	}
	e.awaitProjected("issue", 5, step)

	step = time.Now()
	acked, err := e.client.AcknowledgePurchaseOrder(e.as(e.requester, admin),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || acked.GetStatus() != "acknowledged" {
		t.Fatalf("acknowledge = (%s, %v)", acked.GetStatus(), err)
	}
	e.awaitProjected("acknowledge", 6, step)

	// --- full goods receipt → PO received ----------------------------------
	// The receipt lines reference po_item ids: the convert response echoes
	// the copied basket WITHOUT the DB-generated ids (insertPO returns the
	// aggregate's items), so the flow reads them from the acknowledge
	// response — the lifecycle loads the PO under lock with its stored
	// items (the #38 detail read is the other id-bearing read).
	poItemID := acked.GetItems()[0].GetId()
	if poItemID == "" {
		t.Fatal("acknowledged PO carries no po_item id")
	}
	step = time.Now()
	grn, err := e.client.PostGoodsReceipt(e.as(e.requester, admin),
		&procurementv1.PostGoodsReceiptRequest{
			PoId:  po.GetId(),
			Notes: "p2p full receipt",
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: poItemID, QtyReceived: 2},
			},
		})
	if err != nil {
		t.Fatalf("post goods receipt: %v", err)
	}
	if grn.GetGoodsReceipt().GetNumber() != "GRN-000001" {
		t.Fatalf("GRN number = %s, want GRN-000001", grn.GetGoodsReceipt().GetNumber())
	}
	if grn.GetPurchaseOrder().GetStatus() != "received" {
		t.Fatalf("PO after full receipt = %s, want received", grn.GetPurchaseOrder().GetStatus())
	}
	if got := grn.GetPurchaseOrder().GetItems()[0].GetQtyReceived(); got != 2 {
		t.Fatalf("po_item qty_received = %d, want 2", got)
	}
	e.awaitProjected("grn", 7, step)

	// --- invoice, then the three-way match ---------------------------------
	created, err := e.client.CreateInvoice(e.as(e.requester, admin),
		&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-P2P-0001",
			Items: []*procurementv1.InvoiceItem{
				{PoItemId: poItemID, Qty: 2, UnitPriceCents: 7500000},
			},
		})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if created.GetStatus() != "pending" || created.GetTotalCents() != 15000000 {
		t.Fatalf("created invoice = (%s, %d), want (pending, 15000000)",
			created.GetStatus(), created.GetTotalCents())
	}

	step = time.Now()
	matched, err := e.client.MatchInvoice(e.as(e.requester, admin),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("match invoice: %v", err)
	}
	if matched.GetStatus() != "matched" {
		t.Fatalf("matched invoice status = %s, want matched", matched.GetStatus())
	}
	if !strings.Contains(matched.GetMatchResultJson(), `"matched":true`) {
		t.Fatalf("match_result = %s, want matched:true", matched.GetMatchResultJson())
	}
	e.awaitProjected("match", 8, step)

	// --- the full projected history: exact ordered event set ---------------
	e.assertEventSequence("happy path", happyEventSequence)

	// --- and the closed audit set: five decision rows, nothing else -------
	e.assertAuditSequence("happy path", happyAuditActions)
	rows := e.auditRows()
	wantActors := []string{e.approver, e.approver, e.requester, e.requester, e.requester}
	// invoice.matched audits the INVOICE aggregate (match/store.go), every
	// other decision the requisition/purchase_order it acted on.
	wantEntities := []string{"requisition", "requisition", "purchase_order", "purchase_order", "invoice"}
	for i, row := range rows {
		if row.ActorID != wantActors[i] {
			t.Fatalf("audit row %d actor = %s, want %s", i, row.ActorID, wantActors[i])
		}
		if row.EntityType != wantEntities[i] {
			t.Fatalf("audit row %d entity_type = %s, want %s", i, row.EntityType, wantEntities[i])
		}
	}

	// --- the outbox is fully drained: 8 rows, every one marked published ---
	if got := e.outboxTotal(); got != 8 {
		t.Fatalf("outbox rows = %d, want 8", got)
	}
	if got := e.outboxUnpublished(); got != 0 {
		t.Fatalf("unpublished outbox rows = %d, want 0 (publisher loop drained them)", got)
	}

	e.reportLatencies()
}
