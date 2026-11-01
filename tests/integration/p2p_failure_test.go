package integration

// Failure paths of the procure-to-pay suite (#58, PRD §14.8 / FR-06/FR-07/
// FR-08): every rejection carries its typed gRPC code + machine message AND
// leaves state consistent — the relational rows unchanged and no outbox row
// (hence no projected event) for the failed attempt. Multi-tenancy denial
// (AGENTS: every query scoped by tenant_id, cross-tenant denial mandatory)
// is its own case. Each test runs on its own fresh tenant.

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
)

// setupApproved drives a fresh tenant to an APPROVED requisition: rules,
// vendor, PR with items, submit, approve by the second user. Returns the
// approved requisition (with steps) and the vendor id. Events written: 3
// (pr.submitted, pr.step_approved, pr.approved).
func (e *p2pEnv) setupApproved() (*procurementv1.Requisition, string) {
	e.t.Helper()
	admin := rbac.RoleAdmin
	if _, err := e.client.ReplaceApprovalRules(e.as(e.requester, admin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: rbac.RoleApprover, ThresholdCents: 0},
		}}); err != nil {
		e.t.Fatalf("install approval rules: %v", err)
	}
	v, err := e.client.CreateVendor(e.as(e.requester, admin),
		&procurementv1.CreateVendorRequest{Code: "P2P-58", Name: "PT Procure To Pay"})
	if err != nil {
		e.t.Fatalf("create vendor: %v", err)
	}
	req, err := e.client.CreateRequisition(e.as(e.requester, admin),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: flowItems()})
	if err != nil {
		e.t.Fatalf("create requisition: %v", err)
	}
	submitted, err := e.client.SubmitRequisition(e.as(e.requester, admin),
		&procurementv1.SubmitRequisitionRequest{Id: req.GetId()})
	if err != nil {
		e.t.Fatalf("submit: %v", err)
	}
	decided, err := e.client.DecideApproval(e.as(e.approver, admin),
		&procurementv1.DecideApprovalRequest{
			StepId: submitted.GetApprovalSteps()[0].GetId(), Decision: "approve",
		})
	if err != nil {
		e.t.Fatalf("approve: %v", err)
	}
	return decided.GetRequisition(), v.GetId()
}

// setupAcknowledgedPO drives the tenant further to an ACKNOWLEDGED PO
// (events: 6 — submitted, step_approved, approved, po.created, po.issued,
// po.acknowledged) and waits for the projections so later "no new event"
// asserts compare against a settled baseline.
func (e *p2pEnv) setupAcknowledgedPO() *procurementv1.PurchaseOrder {
	e.t.Helper()
	req, vendorID := e.setupApproved()
	wrapped, err := e.client.CreatePurchaseOrder(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: req.GetId(), VendorId: vendorID})
	if err != nil {
		e.t.Fatalf("convert: %v", err)
	}
	po := wrapped.GetPurchaseOrder()
	if _, err := e.client.IssuePurchaseOrder(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()}); err != nil {
		e.t.Fatalf("issue: %v", err)
	}
	acked, err := e.client.AcknowledgePurchaseOrder(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if err != nil {
		e.t.Fatalf("acknowledge: %v", err)
	}
	e.awaitProjected("setup acknowledged PO", 6, time.Now())
	return acked
}

// (1) Over-receipt is rejected with the typed detail line and writes
// nothing: no GRN rows, no po_items advance, no seventh outbox row, no
// grn.posted projection (FR-07).
func TestFailureGRNOverReceipt(t *testing.T) {
	e := newP2P(t)
	po := e.setupAcknowledgedPO()
	if got := e.outboxTotal(); got != 6 {
		t.Fatalf("baseline outbox = %d, want 6", got)
	}

	_, err := e.client.PostGoodsReceipt(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.PostGoodsReceiptRequest{
			PoId: po.GetId(),
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: po.GetItems()[0].GetId(), QtyReceived: 3}, // ordered 2
			},
		})
	if status.Code(err) != codes.Aborted || !strings.HasPrefix(status.Convert(err).Message(), "grn_over_receipt") {
		t.Fatalf("over-receipt: got (%v, %q), want Aborted grn_over_receipt…",
			status.Code(err), status.Convert(err).Message())
	}

	// State: nothing landed anywhere.
	if got := e.grnTotal(); got != 0 {
		t.Fatalf("goods receipts after rejection = %d, want 0", got)
	}
	if got := e.outboxTotal(); got != 6 {
		t.Fatalf("outbox after rejection = %d, want 6", got)
	}
	after, err := e.client.GetPurchaseOrder(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.GetPurchaseOrderRequest{Id: po.GetId()})
	if err != nil || after.GetStatus() != "acknowledged" {
		t.Fatalf("PO after rejection = (%s, %v), want acknowledged", after.GetStatus(), err)
	}
	if got := after.GetItems()[0].GetQtyReceived(); got != 0 {
		t.Fatalf("qty_received after rejection = %d, want 0", got)
	}
	for _, ev := range e.events() {
		if ev.Type == "grn.posted" {
			t.Fatalf("grn.posted projected although the receipt was rejected (%+v)", ev)
		}
	}
}

// (2) Converting a NOT-approved requisition (still draft) is
// po_requisition_not_approved — the pinned gate — and burns nothing: no PO,
// no po.created row, requisition untouched.
func TestFailureConvertUnapprovedRequisition(t *testing.T) {
	e := newP2P(t)
	admin := rbac.RoleAdmin
	if _, err := e.client.ReplaceApprovalRules(e.as(e.requester, admin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: rbac.RoleApprover, ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("install approval rules: %v", err)
	}
	v, err := e.client.CreateVendor(e.as(e.requester, admin),
		&procurementv1.CreateVendorRequest{Code: "P2P-58", Name: "PT Procure To Pay"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	draft, err := e.client.CreateRequisition(e.as(e.requester, admin),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: flowItems()})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}

	_, err = e.client.CreatePurchaseOrder(e.as(e.requester, admin),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: draft.GetId(), VendorId: v.GetId()})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_requisition_not_approved" {
		t.Fatalf("draft convert: got (%v, %q), want Aborted po_requisition_not_approved",
			status.Code(err), status.Convert(err).Message())
	}

	if got := e.poTotal(); got != 0 {
		t.Fatalf("purchase orders after rejected convert = %d, want 0", got)
	}
	if got := e.outboxTotal(); got != 0 {
		t.Fatalf("outbox after rejected convert = %d, want 0", got)
	}
	after, err := e.client.GetRequisition(e.as(e.requester, admin),
		&procurementv1.GetRequisitionRequest{Id: draft.GetId()})
	if err != nil || after.GetStatus() != "draft" {
		t.Fatalf("draft after rejected convert = (%s, %v), want draft", after.GetStatus(), err)
	}
}

// (3) Self-approval is forbidden even for the admin requester (#30 rule);
// the step stays pending and no decision event is written.
func TestFailureSelfApprovalForbiddenEvenForAdmin(t *testing.T) {
	e := newP2P(t)
	admin := rbac.RoleAdmin
	if _, err := e.client.ReplaceApprovalRules(e.as(e.requester, admin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: rbac.RoleApprover, ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("install approval rules: %v", err)
	}
	req, err := e.client.CreateRequisition(e.as(e.requester, admin),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: flowItems()})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	submitted, err := e.client.SubmitRequisition(e.as(e.requester, admin),
		&procurementv1.SubmitRequisitionRequest{Id: req.GetId()})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	stepID := submitted.GetApprovalSteps()[0].GetId()

	_, err = e.client.DecideApproval(e.as(e.requester, admin),
		&procurementv1.DecideApprovalRequest{StepId: stepID, Decision: "approve"})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "approval_self_forbidden" {
		t.Fatalf("self approve as admin: got (%v, %q), want PermissionDenied approval_self_forbidden",
			status.Code(err), status.Convert(err).Message())
	}

	after, err := e.client.GetRequisition(e.as(e.requester, admin),
		&procurementv1.GetRequisitionRequest{Id: req.GetId()})
	if err != nil || after.GetStatus() != "submitted" || after.GetApprovalSteps()[0].GetStatus() != "pending" {
		t.Fatalf("after self-approval attempt = (%s, %s, %v), want (submitted, pending)",
			after.GetStatus(), after.GetApprovalSteps()[0].GetStatus(), err)
	}
	if got := e.outboxTotal(); got != 1 { // pr.submitted only
		t.Fatalf("outbox after self-approval attempt = %d, want 1", got)
	}
	if rows := len(e.auditRows()); rows != 0 {
		t.Fatalf("audit rows after self-approval attempt = %d, want 0", rows)
	}
}

// (4) Cross-tenant denial (AGENTS multi-tenancy convention): tenant B
// cannot get or decide tenant A's requisition — the rows are
// indistinguishable from missing (NotFound), and tenant A's state is
// untouched afterwards.
func TestFailureCrossTenantDenied(t *testing.T) {
	e := newP2P(t)
	admin := rbac.RoleAdmin
	if _, err := e.client.ReplaceApprovalRules(e.as(e.requester, admin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: rbac.RoleApprover, ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("install approval rules: %v", err)
	}
	req, err := e.client.CreateRequisition(e.as(e.requester, admin),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: flowItems()})
	if err != nil {
		t.Fatalf("create requisition: %v", err)
	}
	submitted, err := e.client.SubmitRequisition(e.as(e.requester, admin),
		&procurementv1.SubmitRequisitionRequest{Id: req.GetId()})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	stepID := submitted.GetApprovalSteps()[0].GetId()
	e.awaitProjected("submit", 1, time.Now()) // let pr.submitted project before the probes

	// Tenant B reads: not found.
	if _, err := e.client.GetRequisition(e.asTenant(e.tenantB, e.approver, admin),
		&procurementv1.GetRequisitionRequest{Id: req.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant get: code = %v, want NotFound", status.Code(err))
	}
	// Tenant B decides: the step is invisible, also not found — and the
	// rejection leaks no existence information.
	if _, err := e.client.DecideApproval(e.asTenant(e.tenantB, e.approver, admin),
		&procurementv1.DecideApprovalRequest{StepId: stepID, Decision: "approve"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant decide: code = %v, want NotFound", status.Code(err))
	}
	// Tenant B converts: the requisition is invisible there too.
	if _, err := e.client.CreatePurchaseOrder(e.asTenant(e.tenantB, e.approver, admin),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: req.GetId(), VendorId: "dddddddd-0000-0000-0000-0000000000be"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant convert: code = %v, want NotFound", status.Code(err))
	}

	// Tenant A's state is untouched by every probe.
	after, err := e.client.GetRequisition(e.as(e.requester, admin),
		&procurementv1.GetRequisitionRequest{Id: req.GetId()})
	if err != nil || after.GetStatus() != "submitted" || after.GetApprovalSteps()[0].GetStatus() != "pending" {
		t.Fatalf("tenant A requisition after probes = (%s, %s, %v), want (submitted, pending)",
			after.GetStatus(), after.GetApprovalSteps()[0].GetStatus(), err)
	}
	// And tenant B owns no rows at all.
	if got := e.dbInt(`SELECT count(*) FROM procurement.requisitions WHERE tenant_id = $1`, e.tenantB); got != 0 {
		t.Fatalf("tenant B requisitions = %d, want 0", got)
	}
	// The Mongo projection stays tenant-scoped: A's one event, B's none.
	if got := len(e.events()); got != 1 {
		t.Fatalf("tenant A projected events = %d, want 1 (pr.submitted)", got)
	}
}

// (5) Invalid lifecycle transition: a cancelled PO accepts no receipt —
// Aborted po_illegal_transition, no GRN rows, and the only new event is the
// po.cancelled of the cancel itself (FR-06).
func TestFailureCancelledPORejectsReceipt(t *testing.T) {
	e := newP2P(t)
	po := e.setupAcknowledgedPO()

	cancelled, err := e.client.CancelPurchaseOrder(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.CancelPurchaseOrderRequest{Id: po.GetId()})
	if err != nil || cancelled.GetStatus() != "cancelled" {
		t.Fatalf("cancel = (%s, %v), want cancelled", cancelled.GetStatus(), err)
	}
	e.awaitProjected("cancel", 7, time.Now()) // + po.cancelled

	_, err = e.client.PostGoodsReceipt(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.PostGoodsReceiptRequest{
			PoId: po.GetId(),
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: po.GetItems()[0].GetId(), QtyReceived: 1},
			},
		})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_illegal_transition" {
		t.Fatalf("receipt on cancelled PO: got (%v, %q), want Aborted po_illegal_transition",
			status.Code(err), status.Convert(err).Message())
	}

	if got := e.grnTotal(); got != 0 {
		t.Fatalf("goods receipts = %d, want 0", got)
	}
	if got := e.outboxTotal(); got != 7 {
		t.Fatalf("outbox = %d, want 7 (nothing beyond po.cancelled)", got)
	}
	// The projected history: the acknowledged flow plus po.cancelled — and
	// no grn.posted anywhere.
	e.assertEventSequence("cancelled PO", []string{
		"pr.submitted", "pr.step_approved", "pr.approved",
		"po.created", "po.issued", "po.acknowledged", "po.cancelled",
	})
}

// (6) Invoice discrepancy: a partial receipt (1 of 2) then a full-quantity
// invoice (2 of 2, tolerance 0%) trips the cumulative qty guard — status
// discrepancy, exactly one invoice.discrepancy event, which IS an audit row
// (action "match", audit.go maps both match outcomes) (FR-08).
func TestFailureInvoiceDiscrepancy(t *testing.T) {
	e := newP2P(t)
	po := e.setupAcknowledgedPO()

	// Partial receipt: 1 of the ordered 2 — allowed, PO partially_received.
	grn, err := e.client.PostGoodsReceipt(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.PostGoodsReceiptRequest{
			PoId: po.GetId(),
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: po.GetItems()[0].GetId(), QtyReceived: 1},
			},
		})
	if err != nil || grn.GetPurchaseOrder().GetStatus() != "partially_received" {
		t.Fatalf("partial receipt = (%s, %v)", grn.GetPurchaseOrder().GetStatus(), err)
	}
	e.awaitProjected("partial grn", 7, time.Now())

	// Full-quantity invoice passes creation (cumulative invoiced total =
	// PO total, the over-bill guard is amount-based)…
	inv, err := e.client.CreateInvoice(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-P2P-DISC-1",
			Items: []*procurementv1.InvoiceItem{
				{PoItemId: po.GetItems()[0].GetId(), Qty: 2, UnitPriceCents: 7500000},
			},
		})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	// …but the three-way match trips the qty guard: invoiced 2 > received 1.
	matched, err := e.client.MatchInvoice(e.as(e.requester, rbac.RoleAdmin),
		&procurementv1.MatchInvoiceRequest{Id: inv.GetId()})
	if err != nil {
		t.Fatalf("match invoice: %v", err)
	}
	if matched.GetStatus() != "discrepancy" {
		t.Fatalf("invoice status = %s, want discrepancy", matched.GetStatus())
	}
	if !strings.Contains(matched.GetMatchResultJson(), "inv_over_bill_qty") {
		t.Fatalf("match_result = %s, want the qty-breach reason", matched.GetMatchResultJson())
	}
	e.awaitProjected("discrepancy", 8, time.Now()) // + invoice.discrepancy

	// The projected history and the audit set: discrepancy is a decision
	// event (action "match"), so five audit rows — the happy path's set.
	e.assertEventSequence("discrepancy", []string{
		"pr.submitted", "pr.step_approved", "pr.approved",
		"po.created", "po.issued", "po.acknowledged", "grn.posted", "invoice.discrepancy",
	})
	e.assertAuditSequence("discrepancy", []string{"approve", "approve", "issue", "post", "match"})
}
