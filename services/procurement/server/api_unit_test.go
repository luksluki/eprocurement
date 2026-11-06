// DB-free unit tests for the procurement API layer (#118): the handler gate,
// per-RPC validation, list-filter validation, the service-layer bounds
// (idempotency key, reason, notes), tenant scoping through tenant-keyed
// fakes (the RLS twin), and store-error → gRPC-code mapping — all against
// the api struct directly with in-memory fakes, no TEST_DATABASE_URL, no
// listener. The full-stack wiring (interceptor RBAC, transport, outbox
// effects) stays in api_test.go's integration suite.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
)

// newUnitAPI wires the api struct over fresh fakes (the handler's own view —
// no transport, no interceptor).
func newUnitAPI() (*api, *fakes) {
	f := newFakes()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &api{
		store: f.vendors, requisitions: f.reqs, approvals: f.approvals,
		purchaseOrders: f.pos, goodsReceipts: f.grns, invoices: f.invoices,
		matches: f.matches, dashboards: f.dashboards, exports: f.exports, log: log,
	}, f
}

// ctxA/ctxB are tenant A/B callers; asA runs as a specific user (ownership).
func ctxA(roles ...string) context.Context { return unitCtx(gUser, gTenantA, roles...) }
func ctxB(roles ...string) context.Context { return unitCtx(gUser, gTenantB, roles...) }
func asA(user string, roles ...string) context.Context {
	return unitCtx(user, gTenantA, roles...)
}

func wantErr(t *testing.T, name string, err error, code codes.Code, msg string) {
	t.Helper()
	if status.Code(err) != code || status.Convert(err).Message() != msg {
		t.Errorf("%s: got (%v, %q), want (%v, %q)",
			name, status.Code(err), status.Convert(err).Message(), code, msg)
	}
}

func unitItems() []*procurementv1.RequisitionItem {
	return []*procurementv1.RequisitionItem{
		{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
		{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
	}
}

// domainItems mirrors unitItems on the domain side (fake-seeding input).
func domainItems() []requisition.Item {
	return []requisition.Item{
		{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
		{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
	}
}

// TestUnitUnauthenticatedGate: every RPC rejects a call with no caller
// metadata before any store is touched (authTenant), the handler half of the
// interceptor's re-check.
func TestUnitUnauthenticatedGate(t *testing.T) {
	a, f := newUnitAPI()
	ctx := context.Background()
	calls := map[string]func() error{
		"CreateVendor": func() error {
			_, err := a.CreateVendor(ctx, &procurementv1.CreateVendorRequest{Code: "V-1", Name: "PT X"})
			return err
		},
		"GetVendor": func() error {
			_, err := a.GetVendor(ctx, &procurementv1.GetVendorRequest{Id: "x"})
			return err
		},
		"UpdateVendor": func() error {
			_, err := a.UpdateVendor(ctx, &procurementv1.UpdateVendorRequest{Id: "x", Name: "PT X"})
			return err
		},
		"DeactivateVendor": func() error {
			_, err := a.DeactivateVendor(ctx, &procurementv1.DeactivateVendorRequest{Id: "x"})
			return err
		},
		"ListVendors": func() error {
			_, err := a.ListVendors(ctx, &procurementv1.ListVendorsRequest{})
			return err
		},
		"CreateRequisition": func() error {
			_, err := a.CreateRequisition(ctx, &procurementv1.CreateRequisitionRequest{Currency: "IDR"})
			return err
		},
		"UpdateRequisition": func() error {
			_, err := a.UpdateRequisition(ctx, &procurementv1.UpdateRequisitionRequest{Id: "x"})
			return err
		},
		"SubmitRequisition": func() error {
			_, err := a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: "x"})
			return err
		},
		"ListRequisitions": func() error {
			_, err := a.ListRequisitions(ctx, &procurementv1.ListRequisitionsRequest{})
			return err
		},
		"GetRequisition": func() error {
			_, err := a.GetRequisition(ctx, &procurementv1.GetRequisitionRequest{Id: "x"})
			return err
		},
		"DecideApproval": func() error {
			_, err := a.DecideApproval(ctx, &procurementv1.DecideApprovalRequest{StepId: "x", Decision: "approve"})
			return err
		},
		"GetApprovalRules": func() error {
			_, err := a.GetApprovalRules(ctx, &procurementv1.GetApprovalRulesRequest{})
			return err
		},
		"ReplaceApprovalRules": func() error {
			_, err := a.ReplaceApprovalRules(ctx, &procurementv1.ReplaceApprovalRulesRequest{})
			return err
		},
		"CreatePurchaseOrder": func() error {
			_, err := a.CreatePurchaseOrder(ctx, &procurementv1.CreatePurchaseOrderRequest{})
			return err
		},
		"IssuePurchaseOrder": func() error {
			_, err := a.IssuePurchaseOrder(ctx, &procurementv1.IssuePurchaseOrderRequest{Id: "x"})
			return err
		},
		"AcknowledgePurchaseOrder": func() error {
			_, err := a.AcknowledgePurchaseOrder(ctx, &procurementv1.AcknowledgePurchaseOrderRequest{Id: "x"})
			return err
		},
		"CancelPurchaseOrder": func() error {
			_, err := a.CancelPurchaseOrder(ctx, &procurementv1.CancelPurchaseOrderRequest{Id: "x"})
			return err
		},
		"ClosePurchaseOrder": func() error {
			_, err := a.ClosePurchaseOrder(ctx, &procurementv1.ClosePurchaseOrderRequest{Id: "x"})
			return err
		},
		"ListPurchaseOrders": func() error {
			_, err := a.ListPurchaseOrders(ctx, &procurementv1.ListPurchaseOrdersRequest{})
			return err
		},
		"GetPurchaseOrder": func() error {
			_, err := a.GetPurchaseOrder(ctx, &procurementv1.GetPurchaseOrderRequest{Id: "x"})
			return err
		},
		"PostGoodsReceipt": func() error {
			_, err := a.PostGoodsReceipt(ctx, &procurementv1.PostGoodsReceiptRequest{PoId: "x"})
			return err
		},
		"CreateInvoice": func() error {
			_, err := a.CreateInvoice(ctx, &procurementv1.CreateInvoiceRequest{PoId: "x", Number: "INV-1"})
			return err
		},
		"GetInvoice": func() error {
			_, err := a.GetInvoice(ctx, &procurementv1.GetInvoiceRequest{Id: "x"})
			return err
		},
		"ListInvoices": func() error {
			_, err := a.ListInvoices(ctx, &procurementv1.ListInvoicesRequest{})
			return err
		},
		"MatchInvoice": func() error {
			_, err := a.MatchInvoice(ctx, &procurementv1.MatchInvoiceRequest{Id: "x"})
			return err
		},
		"GetMatchSettings": func() error {
			_, err := a.GetMatchSettings(ctx, &procurementv1.GetMatchSettingsRequest{})
			return err
		},
		"ReplaceMatchSettings": func() error {
			_, err := a.ReplaceMatchSettings(ctx, &procurementv1.ReplaceMatchSettingsRequest{})
			return err
		},
		"GetDashboard": func() error {
			_, err := a.GetDashboard(ctx, &procurementv1.GetDashboardRequest{})
			return err
		},
		"TriggerExport": func() error {
			_, err := a.TriggerExport(ctx, &procurementv1.TriggerExportRequest{})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: code = %v, want Unauthenticated", name, status.Code(err))
		}
	}
	// No store was touched on the gated path.
	for name, recorded := range map[string][]string{
		"vendors":    f.vendors.tenants,
		"reqs":       f.reqs.tenants,
		"approvals":  f.approvals.tenants,
		"pos":        f.pos.tenants,
		"grns":       f.grns.tenants,
		"invoices":   f.invoices.tenants,
		"matches":    f.matches.tenants,
		"dashboards": f.dashboards.tenants,
		"exports":    f.exports.tenants,
	} {
		if len(recorded) != 0 {
			t.Errorf("%s fake touched without auth context: %v", name, recorded)
		}
	}
}

// TestUnitVendorValidationAndMapping: create-shape validation rejects before
// the store, the happy path normalizes and audits, duplicates map to
// AlreadyExists, and unknown ids to NotFound.
func TestUnitVendorValidationAndMapping(t *testing.T) {
	a, f := newUnitAPI()

	for name, tc := range map[string]struct {
		req *procurementv1.CreateVendorRequest
		msg string
	}{
		"bad code":  {&procurementv1.CreateVendorRequest{Code: "!!", Name: "PT X"}, "vendor_code_invalid"},
		"bad name":  {&procurementv1.CreateVendorRequest{Code: "V-1", Name: ""}, "vendor_name_invalid"},
		"bad email": {&procurementv1.CreateVendorRequest{Code: "V-1", Name: "PT X", ContactEmail: "nope"}, "vendor_email_invalid"},
	} {
		_, err := a.CreateVendor(ctxA(rbac.RoleBuyer), tc.req)
		wantErr(t, name, err, codes.InvalidArgument, tc.msg)
	}
	if n := len(f.vendors.tenants); n != 0 {
		t.Fatalf("store touched by invalid creates: %d calls", n)
	}

	// Happy path: code normalized, caller audited, auth.TenantID forwarded.
	v, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{
		Code: "u-01", Name: "PT Satu", ContactEmail: "c@example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.GetCode() != "U-01" || v.GetStatus() != "active" || v.GetTenantId() != gTenantA ||
		v.GetCreatedBy() != gUser || v.GetUpdatedAt() == "" {
		t.Fatalf("vendor = %+v", v)
	}
	if got := lastTenant(f.vendors.tenants); got != gTenantA {
		t.Fatalf("store tenant = %q, want %q (auth.TenantID must be forwarded)", got, gTenantA)
	}

	// Duplicate code in the same tenant → AlreadyExists with the machine code.
	_, err = a.CreateVendor(ctxA(rbac.RoleAdmin), &procurementv1.CreateVendorRequest{Code: "U-01", Name: "PT Impostor"})
	wantErr(t, "duplicate", err, codes.AlreadyExists, "vendor_duplicate_code")

	// Unknown id → NotFound; deactivated → FailedPrecondition on mutation.
	_, err = a.GetVendor(ctxA(rbac.RoleViewer), &procurementv1.GetVendorRequest{Id: "dddddddd-0000-0000-0000-0000000000ee"})
	wantErr(t, "missing", err, codes.NotFound, "vendor_not_found")
	// Update unknown → NotFound (fake), deactivate twice → FailedPrecondition.
	_, err = a.UpdateVendor(ctxA(rbac.RoleBuyer), &procurementv1.UpdateVendorRequest{Id: "dddddddd-0000-0000-0000-0000000000ee", Name: "PT Y"})
	wantErr(t, "update missing", err, codes.NotFound, "vendor_not_found")
	if _, err := a.DeactivateVendor(ctxA(rbac.RoleBuyer), &procurementv1.DeactivateVendorRequest{Id: v.GetId()}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	_, err = a.UpdateVendor(ctxA(rbac.RoleBuyer), &procurementv1.UpdateVendorRequest{Id: v.GetId(), Name: "PT Zombie"})
	wantErr(t, "update deactivated", err, codes.FailedPrecondition, "vendor_deactivated")
}

// TestUnitVendorIOErrorIsInternal: an unexpected store failure maps to
// Internal with the generic message (no leak), whatever the underlying error.
func TestUnitVendorIOErrorIsInternal(t *testing.T) {
	a, f := newUnitAPI()
	f.vendors.err = errors.New("connection refused")
	_, err := a.GetVendor(ctxA(rbac.RoleViewer), &procurementv1.GetVendorRequest{Id: "dddddddd-0000-0000-0000-000000000001"})
	wantErr(t, "io", err, codes.Internal, "internal error")
}

// TestUnitRequisitionValidation: the domain validation the fake requisition
// store runs (the same pure functions the concrete store calls) surfaces the
// machine codes through the handler's mapper; the happy path normalizes the
// currency and computes the total server-side.
func TestUnitRequisitionValidation(t *testing.T) {
	a, f := newUnitAPI()
	for name, tc := range map[string]struct {
		req *procurementv1.CreateRequisitionRequest
		msg string
	}{
		"no currency":    {&procurementv1.CreateRequisitionRequest{Items: unitItems()}, "requisition_currency_invalid"},
		"bad currency":   {&procurementv1.CreateRequisitionRequest{Currency: "EUR", Items: unitItems()}, "requisition_currency_invalid"},
		"zero qty":       {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "x", Qty: 0}}}, "requisition_qty_invalid"},
		"negative price": {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "x", Qty: 1, UnitPriceCents: -1}}}, "requisition_unit_price_invalid"},
		"blank desc":     {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "  ", Qty: 1}}}, "requisition_description_invalid"},
	} {
		_, err := a.CreateRequisition(ctxA(rbac.RoleRequester), tc.req)
		wantErr(t, name, err, codes.InvalidArgument, tc.msg)
	}

	r, err := a.CreateRequisition(ctxA(rbac.RoleRequester), &procurementv1.CreateRequisitionRequest{
		Currency: " idr ", Items: unitItems(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.GetCurrency() != "IDR" || r.GetStatus() != "draft" || r.GetTotalCents() != 15450000 ||
		r.GetNumber() != "PR-000001" || r.GetRequesterId() != gUser || r.GetTenantId() != gTenantA {
		t.Fatalf("requisition = %+v", r)
	}
	if got := lastTenant(f.reqs.tenants); got != gTenantA {
		t.Fatalf("store tenant = %q, want %q", got, gTenantA)
	}

	// Non-UUID id fails fast with the store's own code.
	_, err = a.GetRequisition(ctxA(rbac.RoleRequester), &procurementv1.GetRequisitionRequest{Id: "not-a-uuid"})
	wantErr(t, "bad id", err, codes.InvalidArgument, "requisition_id_invalid")
}

// TestUnitListFilterValidation: every list rejects an unknown status and an
// absurd page in the domain validators, before the store's List runs.
func TestUnitListFilterValidation(t *testing.T) {
	a, f := newUnitAPI()
	for name, tc := range map[string]struct {
		call func() error
		msg  string
	}{
		"vendor status": {func() error {
			_, err := a.ListVendors(ctxA(rbac.RoleViewer), &procurementv1.ListVendorsRequest{Status: "paused"})
			return err
		}, "vendor_status_invalid"},
		"vendor page": {func() error {
			_, err := a.ListVendors(ctxA(rbac.RoleViewer), &procurementv1.ListVendorsRequest{Page: 2000000})
			return err
		}, "vendor_page_invalid"},
		"requisition status": {func() error {
			_, err := a.ListRequisitions(ctxA(rbac.RoleViewer), &procurementv1.ListRequisitionsRequest{Status: "paused"})
			return err
		}, "requisition_status_invalid"},
		"requisition page": {func() error {
			_, err := a.ListRequisitions(ctxA(rbac.RoleViewer), &procurementv1.ListRequisitionsRequest{Page: 2000000})
			return err
		}, "requisition_page_invalid"},
		"po status": {func() error {
			_, err := a.ListPurchaseOrders(ctxA(rbac.RoleRequester), &procurementv1.ListPurchaseOrdersRequest{Status: "paused"})
			return err
		}, "po_status_invalid"},
		"po page": {func() error {
			_, err := a.ListPurchaseOrders(ctxA(rbac.RoleRequester), &procurementv1.ListPurchaseOrdersRequest{Page: 2000000})
			return err
		}, "po_page_invalid"},
		"invoice status": {func() error {
			_, err := a.ListInvoices(ctxA(rbac.RoleViewer), &procurementv1.ListInvoicesRequest{Status: "paused"})
			return err
		}, "inv_status_invalid"},
		"invoice page": {func() error {
			_, err := a.ListInvoices(ctxA(rbac.RoleViewer), &procurementv1.ListInvoicesRequest{Page: 2000000})
			return err
		}, "inv_page_invalid"},
	} {
		wantErr(t, name, tc.call(), codes.InvalidArgument, tc.msg)
	}
	if n := f.vendors.listCalls; n != 0 {
		t.Fatalf("vendor List ran %d times despite invalid filters", n)
	}
}

// TestUnitServiceLayerBounds: the submit/decide idempotency-key bound, the
// decide reason bound, and the GRN notes bound — the service layer's defense
// in depth, all rejecting before any store call.
func TestUnitServiceLayerBounds(t *testing.T) {
	a, f := newUnitAPI()
	longKey := keyOfLen(65)

	_, err := a.SubmitRequisition(ctxA(rbac.RoleRequester), &procurementv1.SubmitRequisitionRequest{
		Id: "dddddddd-0000-0000-0000-000000000001", IdempotencyKey: longKey,
	})
	wantErr(t, "submit key", err, codes.InvalidArgument, "requisition_idempotency_key_invalid")

	_, err = a.DecideApproval(ctxA(rbac.RoleApprover), &procurementv1.DecideApprovalRequest{
		StepId: "dddddddd-0000-0000-0000-000000000002", Decision: "approve", IdempotencyKey: longKey,
	})
	wantErr(t, "decide key", err, codes.InvalidArgument, "approval_idempotency_key_invalid")

	_, err = a.DecideApproval(ctxA(rbac.RoleApprover), &procurementv1.DecideApprovalRequest{
		StepId: "dddddddd-0000-0000-0000-000000000002", Decision: "reject",
		Reason: strings.Repeat("r", 1001),
	})
	wantErr(t, "reason bound", err, codes.InvalidArgument, "approval_reason_invalid")

	_, err = a.PostGoodsReceipt(ctxA(rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId:  "dddddddd-0000-0000-0000-000000000003",
		Notes: strings.Repeat("n", 1001),
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: "x", QtyReceived: 1}},
	})
	wantErr(t, "notes bound", err, codes.InvalidArgument, "grn_notes_invalid")

	for name, recorded := range map[string][]string{
		"reqs": f.reqs.tenants, "approvals": f.approvals.tenants, "grns": f.grns.tenants,
	} {
		if len(recorded) != 0 {
			t.Errorf("%s fake touched by a bounded rejection: %v", name, recorded)
		}
	}
}

// TestUnitMatchToleranceBounds: 0..100 whole percentages answer the field's
// machine code through the fake's ValidateSettings (the same pure check the
// concrete store runs).
func TestUnitMatchToleranceBounds(t *testing.T) {
	a, _ := newUnitAPI()
	_, err := a.ReplaceMatchSettings(ctxA(rbac.RoleAdmin), &procurementv1.ReplaceMatchSettingsRequest{QtyPct: -1})
	wantErr(t, "qty negative", err, codes.InvalidArgument, "match_qty_pct_invalid")
	_, err = a.ReplaceMatchSettings(ctxA(rbac.RoleAdmin), &procurementv1.ReplaceMatchSettingsRequest{QtyPct: 0, PricePct: 101})
	wantErr(t, "price over", err, codes.InvalidArgument, "match_price_pct_invalid")

	def, err := a.GetMatchSettings(ctxA(rbac.RoleAdmin), &procurementv1.GetMatchSettingsRequest{})
	if err != nil || def.GetQtyPct() != 0 || def.GetPricePct() != 0 {
		t.Fatalf("default settings = (%+v, %v), want 0/0", def, err)
	}
	stored, err := a.ReplaceMatchSettings(ctxA(rbac.RoleAdmin), &procurementv1.ReplaceMatchSettingsRequest{QtyPct: 5, PricePct: 2})
	if err != nil || stored.GetQtyPct() != 5 || stored.GetPricePct() != 2 {
		t.Fatalf("replace = (%+v, %v)", stored, err)
	}
}

// TestUnitTenantScoping: the tenant-keyed fakes mirror RLS — cross-tenant
// reads and writes are indistinguishable from missing, on every domain the
// AC names, while the caller's own tenant keeps working.
func TestUnitTenantScoping(t *testing.T) {
	a, f := newUnitAPI()

	// Tenant A fixtures: a vendor, a draft, an approved requisition + PO, an
	// invoice — all through the handlers where possible.
	v, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: "TS-01", Name: "PT TenantScope"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	draft, err := a.CreateRequisition(ctxA(rbac.RoleRequester), &procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: unitItems()})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	po := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "TS-02", domainItems())
	inv := f.state.seedInvoiceRaw(gTenantA, po, "INV-TS", []invoice.Item{
		{POItemID: poItemByQty(t, &po, 2).ID, Qty: 1, UnitPriceCents: 7500000},
	})

	// Cross-tenant read: NotFound, the RLS answer.
	_, err = a.GetVendor(ctxB(rbac.RoleViewer), &procurementv1.GetVendorRequest{Id: v.GetId()})
	wantErr(t, "vendor read", err, codes.NotFound, "vendor_not_found")

	// Cross-tenant writes: each store keys by tenant, so tenant B's caller
	// cannot even see tenant A's ids.
	_, err = a.UpdateRequisition(ctxB(rbac.RoleRequester), &procurementv1.UpdateRequisitionRequest{Id: draft.GetId(), Items: unitItems()})
	wantErr(t, "requisition edit", err, codes.NotFound, "requisition_not_found")
	_, err = a.SubmitRequisition(ctxB(rbac.RoleRequester), &procurementv1.SubmitRequisitionRequest{Id: draft.GetId()})
	wantErr(t, "requisition submit", err, codes.NotFound, "requisition_not_found")
	_, err = a.CreatePurchaseOrder(ctxB(rbac.RoleBuyer), &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: po.RequisitionID, VendorId: v.GetId()})
	wantErr(t, "po convert", err, codes.NotFound, "requisition_not_found")
	_, err = a.PostGoodsReceipt(ctxB(rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId:  po.ID,
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: po.Items[0].ID, QtyReceived: 1}},
	})
	wantErr(t, "grn post", err, codes.NotFound, "po_not_found")
	_, err = a.CreateInvoice(ctxB(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.ID, Number: "INV-B",
		Items: []*procurementv1.InvoiceItem{{PoItemId: po.Items[0].ID, Qty: 1, UnitPriceCents: 1}},
	})
	wantErr(t, "invoice create", err, codes.NotFound, "po_not_found")
	_, err = a.MatchInvoice(ctxB(rbac.RoleFinance), &procurementv1.MatchInvoiceRequest{Id: inv.ID})
	wantErr(t, "match", err, codes.NotFound, "inv_not_found")

	// Tenant B's lists see none of tenant A's rows.
	resp, err := a.ListVendors(ctxB(rbac.RoleViewer), &procurementv1.ListVendorsRequest{})
	if err != nil || resp.GetTotal() != 0 {
		t.Fatalf("tenant B vendor list = (%+v, %v), want total 0", resp, err)
	}
	poResp, err := a.ListPurchaseOrders(ctxB(rbac.RoleRequester), &procurementv1.ListPurchaseOrdersRequest{})
	if err != nil || poResp.GetTotal() != 0 {
		t.Fatalf("tenant B po list = (%+v, %v), want total 0", poResp, err)
	}

	// The stores received the caller's tenant id on the tenant-A paths.
	for name, fake := range map[string][]string{
		"vendors": f.vendors.tenants, "reqs": f.reqs.tenants, "pos": f.pos.tenants,
		"grns": f.grns.tenants, "invoices": f.invoices.tenants, "matches": f.matches.tenants,
	} {
		if got := lastTenant(fake); got != gTenantB {
			t.Errorf("%s store last tenant = %q, want %q (the cross-tenant caller's id)", name, got, gTenantB)
		}
	}

	// The same caller's own tenant still works end to end: tenant B creates
	// its own vendor under the same code tenant A used.
	mirror, err := a.CreateVendor(ctxB(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: "TS-01", Name: "PT Mirror"})
	if err != nil || mirror.GetTenantId() != gTenantB {
		t.Fatalf("tenant B same-code vendor = (%+v, %v)", mirror, err)
	}
}

// TestUnitOverReceiptTypedError: the typed *purchaseorder.OverReceiptError
// the fake's ApplyReceipt produces (the real store's guard) carries the
// pinned detail line verbatim in the Aborted message.
func TestUnitOverReceiptTypedError(t *testing.T) {
	a, f := newUnitAPI()
	po := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "OR-01", domainItems())
	laptop := poItemByQty(t, &po, 2)

	_, err := a.PostGoodsReceipt(ctxA(rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId:  po.ID,
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: laptop.ID, QtyReceived: 3}},
	})
	want := "grn_over_receipt: po_item=" + laptop.ID + " received=0 requested=3 ordered=2"
	wantErr(t, "over receipt", err, codes.Aborted, want)

	// Nothing landed: the PO is untouched, no GRN exists.
	after, getErr := a.GetPurchaseOrder(ctxA(rbac.RoleBuyer), &procurementv1.GetPurchaseOrderRequest{Id: po.ID})
	if getErr != nil || after.GetStatus() != purchaseorder.StatusAcknowledged {
		t.Fatalf("po after over-receipt = (%s, %v), want acknowledged", after.GetStatus(), getErr)
	}
	if n := len(f.state.grns[gTenantA]); n != 0 {
		t.Fatalf("grns after over-receipt = %d, want 0", n)
	}

	// The illegal edge (draft PO) answers the transition code, and unknown
	// lines the membership code.
	_, err = a.PostGoodsReceipt(ctxA(rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId:  po.ID,
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: "dddddddd-0000-0000-0000-0000000000ee", QtyReceived: 1}},
	})
	wantErr(t, "unknown line", err, codes.InvalidArgument, "grn_po_item_unknown")
}

// TestUnitOverBillTypedError: the typed *invoice.OverBillError carries the
// pinned cumulative detail line verbatim in the Aborted message; a duplicate
// number maps to AlreadyExists.
func TestUnitOverBillTypedError(t *testing.T) {
	a, f := newUnitAPI()
	po := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "OB-01", domainItems())
	laptop := poItemByQty(t, &po, 2)

	_, err := a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.ID, Number: "INV-OB",
		Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.ID, Qty: 2, UnitPriceCents: 8000000}},
	})
	wantErr(t, "over bill", err, codes.Aborted,
		"inv_over_bill: po_number="+po.Number+" invoiced=0 requested=16000000 total=15450000")
	if n := len(f.state.invs[gTenantA]); n != 0 {
		t.Fatalf("invoices after over-bill = %d, want 0", n)
	}

	// Happy path: a fitting invoice lands pending; the duplicate number on
	// the same vendor conflicts.
	created, err := a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId:   po.ID,
		Number: " INV/2026/0001 ",
		Items: []*procurementv1.InvoiceItem{
			{PoItemId: laptop.ID, Qty: 2, UnitPriceCents: 7500000},
			{PoItemId: poItemByQty(t, &po, 3).ID, Qty: 1, UnitPriceCents: 150000},
		},
	})
	if err != nil || created.GetStatus() != "pending" || created.GetTotalCents() != 15150000 ||
		created.GetVendorId() != po.VendorID || created.GetCurrency() != "IDR" ||
		created.GetNumber() != "INV/2026/0001" || len(created.GetItems()) != 2 {
		t.Fatalf("invoice = %+v, %v", created, err)
	}
	_, err = a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.ID, Number: "INV/2026/0001",
		Items: []*procurementv1.InvoiceItem{{PoItemId: poItemByQty(t, &po, 3).ID, Qty: 1, UnitPriceCents: 150000}},
	})
	wantErr(t, "duplicate number", err, codes.AlreadyExists, "inv_number_duplicate")
}

// TestUnitSubmitFlow: the requisition lifecycle over the fakes — rules with
// edge normalization, the no-rules 412, the keyed submit with steps in-band,
// the replay, the key conflict, ownership, and the decide flow to approved.
func TestUnitSubmitFlow(t *testing.T) {
	a, f := newUnitAPI()
	ctx := ctxA(rbac.RoleRequester)

	// No rules yet: submit fails with FailedPrecondition and stays draft.
	basket, err := a.CreateRequisition(ctx, &procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: unitItems()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "unit-key-a"})
	wantErr(t, "submit without rules", err, codes.FailedPrecondition, "approval_rules_missing")
	if still, err := a.GetRequisition(ctx, &procurementv1.GetRequisitionRequest{Id: basket.GetId()}); err != nil || still.GetStatus() != "draft" {
		t.Fatalf("after failed submit = (%s, %v), want still draft", still.GetStatus(), err)
	}

	// Empty draft cannot submit: the domain's items guard.
	empty, err := a.CreateRequisition(ctx, &procurementv1.CreateRequisitionRequest{Currency: "IDR"})
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	_, err = a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: empty.GetId()})
	wantErr(t, "empty submit", err, codes.InvalidArgument, "requisition_items_empty")

	// Configure rules through the handler: currency and role normalize.
	rulesResp, err := a.ReplaceApprovalRules(ctxA(rbac.RoleAdmin), &procurementv1.ReplaceApprovalRulesRequest{
		Rules: []*procurementv1.ApprovalRule{
			{Currency: " idr ", StepNo: 1, RoleRequired: " Approver ", ThresholdCents: 5_000_000},
		},
	})
	if err != nil || len(rulesResp.GetRules()) != 1 ||
		rulesResp.GetRules()[0].GetCurrency() != "IDR" ||
		rulesResp.GetRules()[0].GetRoleRequired() != "approver" ||
		rulesResp.GetRules()[0].GetId() == "" {
		t.Fatalf("stored rules = (%+v, %v)", rulesResp.GetRules(), err)
	}
	if got, err := a.GetApprovalRules(ctxA(rbac.RoleAdmin), &procurementv1.GetApprovalRulesRequest{}); err != nil || len(got.GetRules()) != 1 {
		t.Fatalf("get rules = (%+v, %v)", got, err)
	}

	// Ownership: another requester of the tenant may not edit or submit.
	_, err = a.UpdateRequisition(asA(gUserB, rbac.RoleRequester), &procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: unitItems()})
	wantErr(t, "other requester edit", err, codes.PermissionDenied, "requisition_forbidden")
	_, err = a.SubmitRequisition(asA(gUserB, rbac.RoleRequester), &procurementv1.SubmitRequisitionRequest{Id: basket.GetId()})
	wantErr(t, "other requester submit", err, codes.PermissionDenied, "requisition_forbidden")
	// An admin may edit (maintainer decision 2026-10-23).
	if _, err := a.UpdateRequisition(ctxA(rbac.RoleAdmin), &procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: unitItems()}); err != nil {
		t.Fatalf("admin edit: %v", err)
	}

	// Keyed submit: the SAME key that failed on the 412 now names the fresh
	// submit (nothing was recorded), the steps ride in-band.
	submitted, err := a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "unit-key-a"})
	if err != nil || submitted.GetStatus() != "submitted" || submitted.GetSubmittedAt() == "" {
		t.Fatalf("submit = (%+v, %v)", submitted, err)
	}
	if len(submitted.GetApprovalSteps()) != 1 || submitted.GetApprovalSteps()[0].GetId() == "" ||
		submitted.GetApprovalSteps()[0].GetRoleRequired() != "approver" {
		t.Fatalf("submit steps = %+v", submitted.GetApprovalSteps())
	}
	// Replay: the current state, no second transition.
	replayed, err := a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "unit-key-a"})
	if err != nil || replayed.GetStatus() != "submitted" || replayed.GetId() != basket.GetId() ||
		len(replayed.GetApprovalSteps()) != 1 {
		t.Fatalf("replay = (%+v, %v)", replayed, err)
	}
	// The same key on another requisition conflicts.
	_, err = a.SubmitRequisition(ctx, &procurementv1.SubmitRequisitionRequest{Id: empty.GetId(), IdempotencyKey: "unit-key-a"})
	wantErr(t, "key reuse", err, codes.AlreadyExists, "requisition_idempotency_key_conflict")
	// A submitted requisition is immutable.
	_, err = a.UpdateRequisition(ctx, &procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: unitItems()})
	wantErr(t, "edit submitted", err, codes.Aborted, "requisition_illegal_transition")

	// Decide: self-approval is forbidden even for the requester-as-approver.
	step := submitted.GetApprovalSteps()[0]
	_, err = a.DecideApproval(asA(gUser, rbac.RoleApprover), &procurementv1.DecideApprovalRequest{StepId: step.GetId(), Decision: "approve"})
	wantErr(t, "self approve", err, codes.PermissionDenied, "approval_self_forbidden")
	// A pure approver without the step's role is denied.
	_, err = a.DecideApproval(asA(gUserB, rbac.RoleFinance), &procurementv1.DecideApprovalRequest{StepId: step.GetId(), Decision: "approve"})
	wantErr(t, "role forbidden", err, codes.PermissionDenied, "approval_role_forbidden")
	// Approve as another approver: the single step is terminal → approved.
	after, err := a.DecideApproval(asA(gUserB, rbac.RoleApprover), &procurementv1.DecideApprovalRequest{StepId: step.GetId(), Decision: "approve"})
	if err != nil || after.GetRequisition().GetStatus() != "approved" {
		t.Fatalf("decide = (%s, %v)", after.GetRequisition().GetStatus(), err)
	}
	if s := after.GetRequisition().GetApprovalSteps()[0]; s.GetStatus() != "approved" || s.GetDecidedBy() != gUserB || s.GetDecidedAt() == "" {
		t.Fatalf("decided step = %+v", s)
	}
	// Terminal: further decisions are 409s.
	_, err = a.DecideApproval(asA(gUserB, rbac.RoleApprover), &procurementv1.DecideApprovalRequest{StepId: step.GetId(), Decision: "approve"})
	wantErr(t, "decide after terminal", err, codes.Aborted, "approval_step_not_current")

	// Convert to a PO: created=true, lines copied, requisition closed; the
	// replay (different vendor ignored) answers created=false.
	vendorA, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: "SF-A", Name: "PT Satu"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	wrapped, err := a.CreatePurchaseOrder(ctxA(rbac.RoleBuyer), &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: basket.GetId(), VendorId: vendorA.GetId()})
	if err != nil || !wrapped.GetCreated() {
		t.Fatalf("convert = (%v, %v)", wrapped.GetCreated(), err)
	}
	po := wrapped.GetPurchaseOrder()
	if po.GetNumber() != "PO-000001" || po.GetStatus() != "draft" || po.GetTotalCents() != 15450000 ||
		po.GetRequisitionId() != basket.GetId() || len(po.GetItems()) != 2 {
		t.Fatalf("po = %+v", po)
	}
	if closed, err := a.GetRequisition(ctx, &procurementv1.GetRequisitionRequest{Id: basket.GetId()}); err != nil || closed.GetStatus() != "closed" {
		t.Fatalf("requisition after convert = (%s, %v)", closed.GetStatus(), err)
	}
	vendorB, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: "SF-B", Name: "PT Dua"})
	if err != nil {
		t.Fatalf("create vendor B: %v", err)
	}
	replay, err := a.CreatePurchaseOrder(ctxA(rbac.RoleBuyer), &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: basket.GetId(), VendorId: vendorB.GetId()})
	if err != nil || replay.GetCreated() || replay.GetPurchaseOrder().GetId() != po.GetId() ||
		replay.GetPurchaseOrder().GetVendorId() != vendorA.GetId() {
		t.Fatalf("convert replay = (%+v, %v)", replay.GetPurchaseOrder(), err)
	}
	// Gates: a draft requisition cannot convert.
	_, err = a.CreatePurchaseOrder(ctxA(rbac.RoleBuyer), &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: empty.GetId(), VendorId: vendorA.GetId()})
	wantErr(t, "draft convert", err, codes.Aborted, "po_requisition_not_approved")
	// A deactivated vendor cannot convert.
	dead, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: "SF-D", Name: "PT Mati"})
	if err != nil {
		t.Fatalf("create dead vendor: %v", err)
	}
	if _, err := a.DeactivateVendor(ctxA(rbac.RoleBuyer), &procurementv1.DeactivateVendorRequest{Id: dead.GetId()}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	other := f.state.seedApprovedReq(t, gTenantA, gUser, domainItems())
	_, err = a.CreatePurchaseOrder(ctxA(rbac.RoleBuyer), &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: other.ID, VendorId: dead.GetId()})
	wantErr(t, "deactivated vendor", err, codes.FailedPrecondition, "vendor_deactivated")
}

// TestUnitPOLifecycleAndMatchFlow: issue → acknowledge → partial receipt →
// invoice → match discrepancy → late GRN resolves it → re-run guard, plus
// the cancel guard and the lifecycle machine codes.
func TestUnitPOLifecycleAndMatchFlow(t *testing.T) {
	a, f := newUnitAPI()
	po := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "LC-01", domainItems())
	laptop := poItemByQty(t, &po, 2)
	buyer := ctxA(rbac.RoleBuyer)

	// A fresh draft PO for the lifecycle machine codes.
	draft := f.state.seedApprovedReq(t, gTenantA, gUser, domainItems())
	vendor, err := a.CreateVendor(buyer, &procurementv1.CreateVendorRequest{Code: "LC-02", Name: "PT Dua"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	wrapped, err := a.CreatePurchaseOrder(buyer, &procurementv1.CreatePurchaseOrderRequest{
		RequisitionId: draft.ID, VendorId: vendor.GetId()})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	draftPO := wrapped.GetPurchaseOrder()

	// Machine codes before any state moves.
	_, err = a.IssuePurchaseOrder(buyer, &procurementv1.IssuePurchaseOrderRequest{Id: "nope"})
	wantErr(t, "bad po id", err, codes.InvalidArgument, "po_id_invalid")
	_, err = a.IssuePurchaseOrder(buyer, &procurementv1.IssuePurchaseOrderRequest{Id: "dddddddd-0000-0000-0000-0000000000ff"})
	wantErr(t, "unknown po", err, codes.NotFound, "po_not_found")
	_, err = a.AcknowledgePurchaseOrder(buyer, &procurementv1.AcknowledgePurchaseOrderRequest{Id: draftPO.GetId()})
	wantErr(t, "acknowledge draft", err, codes.Aborted, "po_illegal_transition")

	// Draft cancel lands cancelled; a second cancel is the transition 409.
	if _, err := a.CancelPurchaseOrder(buyer, &procurementv1.CancelPurchaseOrderRequest{Id: draftPO.GetId()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = a.CancelPurchaseOrder(buyer, &procurementv1.CancelPurchaseOrderRequest{Id: draftPO.GetId()})
	wantErr(t, "cancel cancelled", err, codes.Aborted, "po_illegal_transition")

	// The seeded acknowledged PO: a receipt pins it (po_has_receipts).
	first, err := a.PostGoodsReceipt(buyer, &procurementv1.PostGoodsReceiptRequest{
		PoId: po.ID, Notes: "dock 3",
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: laptop.ID, QtyReceived: 1}},
	})
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if grn := first.GetGoodsReceipt(); grn.GetNumber() != "GRN-000001" || grn.GetPoId() != po.ID ||
		grn.GetReceivedBy() != gUser || grn.GetNotes() != "dock 3" || len(grn.GetItems()) != 1 {
		t.Fatalf("grn1 = %+v", grn)
	}
	if after := first.GetPurchaseOrder(); after.GetStatus() != purchaseorder.StatusPartiallyReceived {
		t.Fatalf("po after post 1 = %s", after.GetStatus())
	}
	// Cancel from partially_received is an illegal edge (receipts pin the PO:
	// the only exit is close).
	_, err = a.CancelPurchaseOrder(buyer, &procurementv1.CancelPurchaseOrderRequest{Id: po.ID})
	wantErr(t, "cancel partially_received", err, codes.Aborted, "po_illegal_transition")

	// The receipts guard itself: an acknowledged PO whose line already carries
	// a receipt answers po_has_receipts (the integration suite's grpcForcePO
	// stand-in: bump the stored qty directly).
	pinned := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "LC-04", domainItems())
	f.state.pos[gTenantA][pinned.ID].Items[0].QtyReceived = 1
	_, err = a.CancelPurchaseOrder(buyer, &procurementv1.CancelPurchaseOrderRequest{Id: pinned.ID})
	wantErr(t, "cancel pinned", err, codes.Aborted, "po_has_receipts")

	// Invoice the partial receipt: fits the budget, lands pending.
	created, err := a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.ID, Number: "INV-LC",
		Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.ID, Qty: 2, UnitPriceCents: 7500000}},
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	// First match: billed 2, received 1 → the qty breach lands discrepancy.
	discrepant, err := a.MatchInvoice(ctxA(rbac.RoleFinance), &procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	if err != nil || discrepant.GetStatus() != "discrepancy" {
		t.Fatalf("match 1 = (%s, %v)", discrepant.GetStatus(), err)
	}
	if !strings.Contains(discrepant.GetMatchResultJson(), "inv_over_bill_qty") ||
		!strings.Contains(discrepant.GetMatchResultJson(), `"qty_received":1`) {
		t.Fatalf("match result = %s", discrepant.GetMatchResultJson())
	}

	// The late GRN completes the laptop line (mouse still short →
	// same-status branch); the re-run resolves the discrepancy.
	if _, err := a.PostGoodsReceipt(buyer, &procurementv1.PostGoodsReceiptRequest{
		PoId:  po.ID,
		Items: []*procurementv1.GoodsReceiptItem{{PoItemId: laptop.ID, QtyReceived: 1}},
	}); err != nil {
		t.Fatalf("post 2: %v", err)
	}
	resolved, err := a.MatchInvoice(ctxA(rbac.RoleFinance), &procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	if err != nil || resolved.GetStatus() != "matched" ||
		!strings.Contains(resolved.GetMatchResultJson(), `"matched":true`) {
		t.Fatalf("match 2 = (%s, %s, %v)", resolved.GetStatus(), resolved.GetMatchResultJson(), err)
	}

	// The re-run guard: matched is final.
	_, err = a.MatchInvoice(ctxA(rbac.RoleFinance), &procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	wantErr(t, "re-run matched", err, codes.Aborted, "inv_already_matched")

	// Short-close the partially_received PO; nothing leaves closed.
	if _, err := a.ClosePurchaseOrder(buyer, &procurementv1.ClosePurchaseOrderRequest{Id: po.ID}); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err = a.CancelPurchaseOrder(buyer, &procurementv1.CancelPurchaseOrderRequest{Id: po.ID})
	wantErr(t, "cancel closed", err, codes.Aborted, "po_illegal_transition")

	// An invoiced-beyond-budget case on a fresh PO answers over-bill even
	// though the mouse line was never received: the cumulative guard runs
	// before the status edge — pinned order, asserted again on the fake.
	fresh := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "LC-03", domainItems())
	freshLaptop := poItemByQty(t, &fresh, 2)
	freshMouse := poItemByQty(t, &fresh, 3)
	_, err = a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: fresh.ID, Number: "INV-LC-2",
		Items: []*procurementv1.InvoiceItem{
			{PoItemId: freshMouse.ID, Qty: 1, UnitPriceCents: 150000},
			{PoItemId: freshLaptop.ID, Qty: 2, UnitPriceCents: 7500000},
		},
	})
	if err != nil {
		t.Fatalf("fresh invoice: %v", err)
	}
	_, err = a.CreateInvoice(ctxA(rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: fresh.ID, Number: "INV-LC-3",
		Items: []*procurementv1.InvoiceItem{{PoItemId: freshMouse.ID, Qty: 3, UnitPriceCents: 150000}},
	})
	wantErr(t, "cumulative over bill", err, codes.Aborted,
		"inv_over_bill: po_number="+fresh.Number+" invoiced=15150000 requested=450000 total=15450000")
}

// TestUnitReadsDashboardExport: the list/detail reads over the fakes
// (header-only rows, tenant-scoped totals), the dashboard widgets from the
// shared state, and the export trigger's event id.
func TestUnitReadsDashboardExport(t *testing.T) {
	a, f := newUnitAPI()

	// Fixtures through the handlers: two vendors, one submitted requisition
	// (pending step), one issued PO for spend and the distribution.
	for _, code := range []string{"RD-A", "RD-B"} {
		if _, err := a.CreateVendor(ctxA(rbac.RoleBuyer), &procurementv1.CreateVendorRequest{Code: code, Name: "PT " + code}); err != nil {
			t.Fatalf("create vendor %s: %v", code, err)
		}
	}
	if _, err := a.ReplaceApprovalRules(ctxA(rbac.RoleAdmin), &procurementv1.ReplaceApprovalRulesRequest{
		Rules: []*procurementv1.ApprovalRule{{Currency: "IDR", StepNo: 1, RoleRequired: "approver"}},
	}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	// Seed the issued PO first so the API-created pending requisition carries
	// the newest PR number (the list is newest-first).
	issued := f.state.seedAcknowledgedPO(t, gTenantA, gUser, "RD-C", domainItems())
	pending, err := a.CreateRequisition(ctxA(rbac.RoleRequester), &procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: unitItems()})
	if err != nil {
		t.Fatalf("create pending: %v", err)
	}
	if _, err := a.SubmitRequisition(ctxA(rbac.RoleRequester), &procurementv1.SubmitRequisitionRequest{Id: pending.GetId()}); err != nil {
		t.Fatalf("submit pending: %v", err)
	}

	// Vendor list: defaults, code-order, prefix filter.
	resp, err := a.ListVendors(ctxA(rbac.RoleViewer), &procurementv1.ListVendorsRequest{})
	if err != nil || resp.GetPage() != 1 || resp.GetPageSize() != 20 || resp.GetTotal() != 3 ||
		resp.GetVendors()[0].GetCode() != "RD-A" {
		t.Fatalf("vendor list = (%+v, %v)", resp, err)
	}
	prefixed, err := a.ListVendors(ctxA(rbac.RoleViewer), &procurementv1.ListVendorsRequest{Query: "rd-b"})
	if err != nil || prefixed.GetTotal() != 1 || prefixed.GetVendors()[0].GetCode() != "RD-B" {
		t.Fatalf("prefixed = (%+v, %v)", prefixed, err)
	}

	// Requisition list: newest first, header-only; detail owns the bodies.
	list, err := a.ListRequisitions(ctxA(rbac.RoleViewer), &procurementv1.ListRequisitionsRequest{})
	if err != nil || list.GetTotal() != 2 || len(list.GetRequisitions()) != 2 {
		t.Fatalf("req list = (%+v, %v)", list, err)
	}
	if rows := list.GetRequisitions(); rows[0].GetNumber() != pending.GetNumber() {
		t.Fatalf("not newest-first: %+v", rows)
	}
	for _, row := range list.GetRequisitions() {
		if len(row.GetItems()) != 0 || len(row.GetApprovalSteps()) != 0 {
			t.Fatalf("list row carries a body: %+v", row)
		}
	}
	detail, err := a.GetRequisition(ctxA(rbac.RoleViewer), &procurementv1.GetRequisitionRequest{Id: pending.GetId()})
	if err != nil || len(detail.GetItems()) != 2 || len(detail.GetApprovalSteps()) != 1 ||
		detail.GetApprovalSteps()[0].GetStatus() != "pending" {
		t.Fatalf("req detail = (%+v, %v)", detail, err)
	}
	// Status filter narrows to the submitted row.
	onlySubmitted, err := a.ListRequisitions(ctxA(rbac.RoleViewer), &procurementv1.ListRequisitionsRequest{Status: "submitted"})
	if err != nil || onlySubmitted.GetTotal() != 1 || onlySubmitted.GetRequisitions()[0].GetId() != pending.GetId() {
		t.Fatalf("status filter = (%+v, %v)", onlySubmitted, err)
	}

	// PO list/detail: the seeded acknowledged PO carries items with ids.
	poList, err := a.ListPurchaseOrders(ctxA(rbac.RoleRequester), &procurementv1.ListPurchaseOrdersRequest{})
	if err != nil || poList.GetTotal() != 1 || len(poList.GetPurchaseOrders()) != 1 ||
		len(poList.GetPurchaseOrders()[0].GetItems()) != 0 {
		t.Fatalf("po list = (%+v, %v)", poList, err)
	}
	poDetail, err := a.GetPurchaseOrder(ctxA(rbac.RoleFinance), &procurementv1.GetPurchaseOrderRequest{Id: issued.ID})
	if err != nil || len(poDetail.GetItems()) != 2 {
		t.Fatalf("po detail = (%+v, %v)", poDetail, err)
	}
	for _, it := range poDetail.GetItems() {
		if it.GetId() == "" {
			t.Fatalf("po detail item carries no id: %+v", it)
		}
	}

	// Invoices: header-only list rows even after a stored verdict; the
	// detail owns items and match_result.
	inv := f.state.seedInvoiceRaw(gTenantA, issued, "INV-RD", []invoice.Item{
		{POItemID: poItemByQty(t, &issued, 2).ID, Qty: 1, UnitPriceCents: 7500000},
	})
	if _, err := a.MatchInvoice(ctxA(rbac.RoleFinance), &procurementv1.MatchInvoiceRequest{Id: inv.ID}); err != nil {
		t.Fatalf("match: %v", err)
	}
	invList, err := a.ListInvoices(ctxA(rbac.RoleViewer), &procurementv1.ListInvoicesRequest{})
	if err != nil || invList.GetTotal() != 1 || len(invList.GetInvoices()) != 1 {
		t.Fatalf("inv list = (%+v, %v)", invList, err)
	}
	if row := invList.GetInvoices()[0]; len(row.GetItems()) != 0 || row.GetMatchResultJson() != "" {
		t.Fatalf("inv list row carries a body: %+v", row)
	}
	invDetail, err := a.GetInvoice(ctxA(rbac.RoleViewer), &procurementv1.GetInvoiceRequest{Id: inv.ID})
	if err != nil || len(invDetail.GetItems()) != 1 || invDetail.GetMatchResultJson() == "" {
		t.Fatalf("inv detail = (%+v, %v)", invDetail, err)
	}

	// Dashboard: pending 1 (the submitted requisition's current step), the
	// acknowledged PO in the distribution, spend in both windows.
	summary, err := a.GetDashboard(ctxA(rbac.RoleViewer), &procurementv1.GetDashboardRequest{})
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if summary.GetPendingApprovals().GetTotal() != 1 || len(summary.GetPendingApprovals().GetItems()) != 1 ||
		summary.GetPendingApprovals().GetItems()[0].GetRequisitionId() != pending.GetId() ||
		summary.GetPendingApprovals().GetItems()[0].GetRoleRequired() != "approver" {
		t.Fatalf("pending widget = %+v", summary.GetPendingApprovals())
	}
	distOK := false
	for _, d := range summary.GetPoStatusDistribution() {
		if d.GetStatus() == purchaseorder.StatusAcknowledged && d.GetCount() == 1 {
			distOK = true
		}
	}
	if !distOK || len(summary.GetPoStatusDistribution()) != 7 {
		t.Fatalf("distribution = %+v", summary.GetPoStatusDistribution())
	}
	if len(summary.GetSpendByVendor()) != 1 || summary.GetSpendByVendor()[0].GetCurrency() != "IDR" ||
		summary.GetSpendByVendor()[0].GetThirtyDayCents() != issued.TotalCents {
		t.Fatalf("spend = %+v", summary.GetSpendByVendor())
	}
	// Cross-tenant: tenant B's summary is an empty snapshot, not a leak.
	other, err := a.GetDashboard(ctxB(rbac.RoleViewer), &procurementv1.GetDashboardRequest{})
	if err != nil || other.GetPendingApprovals().GetTotal() != 0 || len(other.GetSpendByVendor()) != 0 ||
		len(other.GetPoStatusDistribution()) != 7 {
		t.Fatalf("tenant B summary = (%+v, %v)", other, err)
	}

	// Export: a fresh event id per trigger, recorded under the caller's
	// tenant; tenant B's trigger lands in tenant B's ledger.
	first, err := a.TriggerExport(ctxA(rbac.RoleAdmin), &procurementv1.TriggerExportRequest{})
	if err != nil || first.GetEventId() == "" {
		t.Fatalf("export 1 = (%+v, %v)", first, err)
	}
	second, err := a.TriggerExport(ctxA(rbac.RoleAdmin), &procurementv1.TriggerExportRequest{})
	if err != nil || second.GetEventId() == first.GetEventId() {
		t.Fatalf("export 2 = (%+v, %v), want a distinct id", second, err)
	}
	if _, err := a.TriggerExport(ctxB(rbac.RoleAdmin), &procurementv1.TriggerExportRequest{}); err != nil {
		t.Fatalf("export tenant B: %v", err)
	}
	if n := len(f.state.exportRequests[gTenantA]); n != 2 {
		t.Fatalf("tenant A export rows = %d, want 2", n)
	}
	if n := len(f.state.exportRequests[gTenantB]); n != 1 {
		t.Fatalf("tenant B export rows = %d, want 1", n)
	}
}

// TestUnitNewAcceptsFakes: the server constructor takes the fakes (the
// interfaces' whole point) and registers the API — no database involved.
func TestUnitNewAcceptsFakes(t *testing.T) {
	f := newFakes()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := New(log, fakePinger{}, f.vendors, f.reqs, f.approvals, f.pos,
		f.grns, f.invoices, f.matches, f.dashboards, f.exports)
	if srv == nil || srv.GRPC() == nil {
		t.Fatal("New over fakes returned no server")
	}
}

// Compile-time proofs that the fakes satisfy the extracted interfaces.
var (
	_ VendorStore        = (*fakeVendorStore)(nil)
	_ RequisitionStore   = (*fakeRequisitionStore)(nil)
	_ ApprovalStore      = (*fakeApprovalStore)(nil)
	_ PurchaseOrderStore = (*fakePurchaseOrderStore)(nil)
	_ GoodsReceiptStore  = (*fakeGoodsReceiptStore)(nil)
	_ InvoiceStore       = (*fakeInvoiceStore)(nil)
	_ MatchStore         = (*fakeMatchStore)(nil)
	_ DashboardStore     = (*fakeDashboardStore)(nil)
	_ ExportStore        = (*fakeExportStore)(nil)
)
