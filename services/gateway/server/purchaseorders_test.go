package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
)

// fakePOClient satisfies the widened ProcurementClient (vendor +
// requisition + approval via the embedded fakes) and records what the
// gateway forwarded to the conversion call and to the lifecycle calls.
type fakePOClient struct {
	*fakeRequisitionClient

	poReq *procurementv1.CreatePurchaseOrderRequest
	poRes *procurementv1.CreatePurchaseOrderResponse
	poErr error
	poCtx context.Context

	trReq string // the id forwarded to the last lifecycle call
	trRes *procurementv1.PurchaseOrder
	trErr error
	trCtx context.Context

	grnReq *procurementv1.PostGoodsReceiptRequest
	grnRes *procurementv1.PostGoodsReceiptResponse
	grnErr error
	grnCtx context.Context

	listReq *procurementv1.ListPurchaseOrdersRequest
	listRes *procurementv1.ListPurchaseOrdersResponse
	listErr error
	listCtx context.Context

	getReq *procurementv1.GetPurchaseOrderRequest
	getRes *procurementv1.PurchaseOrder
	getErr error
	getCtx context.Context
}

func (f *fakePOClient) CreatePurchaseOrder(ctx context.Context, in *procurementv1.CreatePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.CreatePurchaseOrderResponse, error) {
	f.poCtx, f.poReq = ctx, in
	if f.poErr != nil {
		return nil, f.poErr
	}
	return f.poRes, nil
}

// transition records the lifecycle calls (they all carry only the id).
func (f *fakePOClient) transition(ctx context.Context, id string) (*procurementv1.PurchaseOrder, error) {
	f.trCtx, f.trReq = ctx, id
	if f.trErr != nil {
		return nil, f.trErr
	}
	return f.trRes, nil
}

func (f *fakePOClient) IssuePurchaseOrder(ctx context.Context, in *procurementv1.IssuePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return f.transition(ctx, in.GetId())
}

func (f *fakePOClient) AcknowledgePurchaseOrder(ctx context.Context, in *procurementv1.AcknowledgePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return f.transition(ctx, in.GetId())
}

func (f *fakePOClient) CancelPurchaseOrder(ctx context.Context, in *procurementv1.CancelPurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return f.transition(ctx, in.GetId())
}

func (f *fakePOClient) ClosePurchaseOrder(ctx context.Context, in *procurementv1.ClosePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return f.transition(ctx, in.GetId())
}

// grnReq/grnRes/grnErr/grnCtx record the goods-receipt posting calls (#37).
func (f *fakePOClient) PostGoodsReceipt(ctx context.Context, in *procurementv1.PostGoodsReceiptRequest, opts ...grpc.CallOption) (*procurementv1.PostGoodsReceiptResponse, error) {
	f.grnCtx, f.grnReq = ctx, in
	if f.grnErr != nil {
		return nil, f.grnErr
	}
	return f.grnRes, nil
}

func samplePO() *procurementv1.PurchaseOrder {
	return &procurementv1.PurchaseOrder{
		Id: "po-1", TenantId: "tenant-29", Number: "PO-000001",
		RequisitionId: "pr-1", VendorId: "v-1", Status: "draft",
		Currency: "IDR", TotalCents: 15450000, IssuedAt: "",
		Items: []*procurementv1.PurchaseOrderItem{
			{Description: "Laptop", QtyOrdered: 2, QtyReceived: 0, UnitPriceCents: 7500000},
			{Description: "Mouse", QtyOrdered: 3, QtyReceived: 0, UnitPriceCents: 150000},
		},
		CreatedAt: "2026-10-26T10:00:00Z", CreatedBy: "user-29",
		UpdatedAt: "2026-10-26T10:00:00Z", UpdatedBy: "user-29",
	}
}

// AC: the conversion route is wired through server.New with
// WithProcurement, po:write (buyer) grants access, the path id and the
// trimmed body vendor_id are forwarded, claims ride along as gRPC metadata,
// the draft PO echoes back as 201 on creation — and as 200 with the same
// body when procurement reports a replay (created=false).
func TestPurchaseOrderRouteThroughServerNew(t *testing.T) {
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		poRes:                 &procurementv1.CreatePurchaseOrderResponse{PurchaseOrder: samplePO(), Created: true},
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })

	resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
		reqTokenFor(t, priv, kid, rbac.RoleBuyer), `{"vendor_id":"  v-1  "}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("convert status = %d, body = %v", resp.StatusCode, body)
	}
	if body["number"] != "PO-000001" || body["status"] != "draft" || body["issued_at"] != "" ||
		body["total_cents"] != float64(15450000) || body["requisition_id"] != "pr-1" {
		t.Fatalf("convert body = %v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", body["items"])
	}
	first, _ := items[0].(map[string]any)
	if first["qty_ordered"] != float64(2) || first["qty_received"] != float64(0) ||
		first["unit_price_cents"] != float64(7500000) {
		t.Fatalf("items = %v", items)
	}
	// The path id and the trimmed vendor_id were forwarded.
	if fake.poReq.GetRequisitionId() != "pr-1" || fake.poReq.GetVendorId() != "v-1" {
		t.Fatalf("forwarded request = %+v", fake.poReq)
	}
	// The verified claims were propagated as gRPC metadata.
	md := metadataOf(t, fake.poCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" || md["x-role"] != rbac.RoleBuyer {
		t.Fatalf("propagated metadata = %v", md)
	}

	// A replay (created=false) answers 200 with the same PO body.
	fake.poRes.Created = false
	resp, body = reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
		reqTokenFor(t, priv, kid, rbac.RoleBuyer), `{"vendor_id":"v-1"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, body = %v", resp.StatusCode, body)
	}
	if body["number"] != "PO-000001" || body["status"] != "draft" {
		t.Fatalf("replay body = %v", body)
	}
}

// po:write is buyer-only: requester, approver, finance, and viewer get 403
// before any procurement call.
func TestPurchaseOrderRouteRequiresPermission(t *testing.T) {
	fake := &fakePOClient{fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}}
	app, verifier, priv, kid := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)

	for _, role := range []string{rbac.RoleRequester, rbac.RoleApprover, rbac.RoleFinance, rbac.RoleViewer} {
		resp, _ := reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
			reqTokenFor(t, priv, kid, role), `{"vendor_id":"v-1"}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("convert as %s: status = %d, want 403", role, resp.StatusCode)
		}
	}
	if fake.poReq != nil {
		t.Fatal("procurement was called despite denied permission")
	}

	// The admin role passes the permission gate and reaches procurement.
	resp, _ := reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
		reqTokenFor(t, priv, kid, rbac.RoleAdmin), `{"vendor_id":"v-1"}`, nil)
	if resp.StatusCode == http.StatusForbidden || fake.poReq == nil {
		t.Fatalf("admin convert: status = %d, forwarded = %v", resp.StatusCode, fake.poReq)
	}
}

// The purchase-order slice of the shared gRPC→HTTP mapping: status gate
// (Aborted) → 409, vendor gate (FailedPrecondition) → 412, unknown
// requisition → 404, bad vendor_id → 400 — machine codes always preserved.
// A malformed JSON body is 400 at the edge without a procurement call.
func TestPurchaseOrderRouteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not approved":    {status.Error(codes.Aborted, "po_requisition_not_approved"), http.StatusConflict},
		"vendor gone":     {status.Error(codes.FailedPrecondition, "vendor_deactivated"), http.StatusPreconditionFailed},
		"requisition 404": {status.Error(codes.NotFound, "requisition_not_found"), http.StatusNotFound},
		"bad vendor id":   {status.Error(codes.InvalidArgument, "po_vendor_id_invalid"), http.StatusBadRequest},
	} {
		fake := &fakePOClient{
			fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
			poErr:                 tc.err,
		}
		app, verifier, priv, kid := newTestApp(t)
		registerPurchaseOrderRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleBuyer)

		resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
			token, `{"vendor_id":"v-1"}`, nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the machine code", name, body)
		}
		if human, ok := body["error"].(string); !ok || strings.TrimSpace(human) == "" || human == status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the human sentence", name, body)
		}
	}

	// Malformed JSON: 400 body_invalid without a procurement call.
	fake := &fakePOClient{fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}}
	app, verifier, priv, kid := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)
	resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/from-requisition/pr-1",
		reqTokenFor(t, priv, kid, rbac.RoleBuyer), `{`, nil)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "body_invalid" {
		t.Fatalf("bad body = (%d, %v), want 400 body_invalid", resp.StatusCode, body)
	}
	if fake.poReq != nil {
		t.Fatal("procurement was called despite a malformed body")
	}
}

// AC (#36): the four lifecycle routes are wired through server.New with
// WithProcurement, po:write (buyer) grants access, the path id is forwarded
// with the claims as gRPC metadata, and the updated PO answers 200.
func TestPurchaseOrderLifecycleRoutesThroughServerNew(t *testing.T) {
	issued := samplePO()
	issued.Status = "issued"
	issued.IssuedAt = "2026-10-26T11:00:00Z"
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		trRes:                 issued,
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })

	for _, action := range []string{"issue", "acknowledge", "cancel", "close"} {
		resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/"+action,
			reqTokenFor(t, priv, kid, rbac.RoleBuyer), "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, body = %v", action, resp.StatusCode, body)
		}
		if fake.trReq != "po-9" {
			t.Fatalf("%s forwarded id = %q, want po-9", action, fake.trReq)
		}
		if body["id"] != "po-1" || body["status"] != "issued" || body["issued_at"] != "2026-10-26T11:00:00Z" {
			t.Fatalf("%s body = %v", action, body)
		}
		md := metadataOf(t, fake.trCtx)
		if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" || md["x-role"] != rbac.RoleBuyer {
			t.Fatalf("%s propagated metadata = %v", action, md)
		}
	}
}

// The lifecycle routes inherit po:write (buyer-only): requester, approver,
// finance, and viewer get 403 before any procurement call.
func TestPurchaseOrderLifecycleRoutesRequirePermission(t *testing.T) {
	fake := &fakePOClient{fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}}
	app, verifier, priv, kid := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)

	for _, role := range []string{rbac.RoleRequester, rbac.RoleApprover, rbac.RoleFinance, rbac.RoleViewer} {
		for _, action := range []string{"issue", "acknowledge", "cancel", "close"} {
			resp, _ := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/"+action,
				reqTokenFor(t, priv, kid, role), "", nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s as %s: status = %d, want 403", action, role, resp.StatusCode)
			}
		}
	}
	if fake.trReq != "" {
		t.Fatal("procurement was called despite denied permission")
	}

	// The admin role passes the permission gate and reaches procurement.
	resp, _ := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/issue",
		reqTokenFor(t, priv, kid, rbac.RoleAdmin), "", nil)
	if resp.StatusCode == http.StatusForbidden || fake.trReq == "" {
		t.Fatalf("admin issue: status = %d, forwarded = %q", resp.StatusCode, fake.trReq)
	}
}

// The lifecycle slice of the shared gRPC→HTTP mapping: po_id_invalid → 400,
// po_not_found → 404, po_illegal_transition / po_has_receipts → 409 —
// machine codes always preserved, human sentences always present.
func TestPurchaseOrderLifecycleRouteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"illegal transition": {status.Error(codes.Aborted, "po_illegal_transition"), http.StatusConflict},
		"has receipts":       {status.Error(codes.Aborted, "po_has_receipts"), http.StatusConflict},
		"po 404":             {status.Error(codes.NotFound, "po_not_found"), http.StatusNotFound},
		"bad po id":          {status.Error(codes.InvalidArgument, "po_id_invalid"), http.StatusBadRequest},
	} {
		fake := &fakePOClient{
			fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
			trErr:                 tc.err,
		}
		app, verifier, priv, kid := newTestApp(t)
		registerPurchaseOrderRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleBuyer)

		resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/issue", token, "", nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the machine code", name, body)
		}
		if human, ok := body["error"].(string); !ok || strings.TrimSpace(human) == "" || human == status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the human sentence", name, body)
		}
	}
}

// sampleGRN builds the posting response the wiring test expects back.
func sampleGRN() *procurementv1.PostGoodsReceiptResponse {
	return &procurementv1.PostGoodsReceiptResponse{
		GoodsReceipt: &procurementv1.GoodsReceipt{
			Id: "grn-1", TenantId: "tenant-29", PoId: "po-1", Number: "GRN-000001",
			ReceivedBy: "user-29", ReceivedAt: "2026-10-27T09:00:00Z", Notes: "dock 3",
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: "po-item-1", QtyReceived: 1},
			},
			CreatedAt: "2026-10-27T09:00:00Z",
		},
		PurchaseOrder: func() *procurementv1.PurchaseOrder {
			po := samplePO()
			po.Status = "partially_received"
			po.Items[0].Id = "po-item-1"
			po.Items[0].QtyReceived = 1
			return po
		}(),
	}
}

// AC: the goods-receipt route is wired through server.New with
// WithProcurement, grn:post (buyer) grants access, the path id, the trimmed
// item ids, and the trimmed notes are forwarded, claims ride along as gRPC
// metadata, and the 201 body carries the GRN plus the updated PO embedded —
// its items now exposing the ids receipt lines reference.
func TestGoodsReceiptRouteThroughServerNew(t *testing.T) {
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		grnRes:                sampleGRN(),
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })

	body := `{"items":[{"po_item_id":"  po-item-1  ","qty_received":1}],"notes":"  dock 3  "}`
	resp, parsed := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-1/goods-receipts",
		reqTokenFor(t, priv, kid, rbac.RoleBuyer), body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post status = %d, body = %v", resp.StatusCode, parsed)
	}
	if parsed["number"] != "GRN-000001" || parsed["po_id"] != "po-1" ||
		parsed["received_by"] != "user-29" || parsed["notes"] != "dock 3" {
		t.Fatalf("grn body = %v", parsed)
	}
	items, _ := parsed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", parsed["items"])
	}
	first, _ := items[0].(map[string]any)
	if first["po_item_id"] != "po-item-1" || first["qty_received"] != float64(1) {
		t.Fatalf("items = %v", items)
	}
	embedded, _ := parsed["purchase_order"].(map[string]any)
	if embedded == nil || embedded["status"] != "partially_received" {
		t.Fatalf("purchase_order = %v", parsed["purchase_order"])
	}
	poItems, _ := embedded["items"].([]any)
	poFirst, _ := poItems[0].(map[string]any)
	if poFirst["id"] != "po-item-1" || poFirst["qty_received"] != float64(1) {
		t.Fatalf("po items = %v (id + advanced qty expected)", embedded["items"])
	}
	// The path id, the trimmed item id, and the trimmed notes were forwarded.
	if fake.grnReq.GetPoId() != "po-1" || fake.grnReq.GetNotes() != "dock 3" ||
		len(fake.grnReq.GetItems()) != 1 || fake.grnReq.GetItems()[0].GetPoItemId() != "po-item-1" ||
		fake.grnReq.GetItems()[0].GetQtyReceived() != 1 {
		t.Fatalf("forwarded request = %+v", fake.grnReq)
	}
	// The verified claims were propagated as gRPC metadata.
	md := metadataOf(t, fake.grnCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("metadata = %v", md)
	}
}

// AC: grn:post is buyer-only — every other role is rejected at the gateway
// before any procurement call (nothing forwarded), and the implicit admin
// pass-through holds.
func TestGoodsReceiptRouteRequiresPermission(t *testing.T) {
	for name, role := range map[string]string{
		"requester": rbac.RoleRequester,
		"approver":  rbac.RoleApprover,
		"finance":   rbac.RoleFinance,
		"viewer":    rbac.RoleViewer,
	} {
		fake := &fakePOClient{
			fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
			grnRes:                sampleGRN(),
		}
		app, verifier, priv, kid := newTestApp(t)
		registerPurchaseOrderRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, role)
		resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-1/goods-receipts", token,
			`{"items":[{"po_item_id":"po-item-1","qty_received":1}]}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["required"] != rbac.GRNPost {
			t.Fatalf("%s: body = %v, want required %s", name, body, rbac.GRNPost)
		}
		if fake.grnReq != nil {
			t.Fatalf("%s: procurement called despite the denial", name)
		}
	}
	// Admin passes implicitly (admin holds every permission).
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		grnRes:                sampleGRN(),
	}
	app, verifier, priv, kid := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)
	resp, _ := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-1/goods-receipts",
		reqTokenFor(t, priv, kid, rbac.RoleAdmin), `{"items":[{"po_item_id":"po-item-1","qty_received":1}]}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin: status = %d, want 201", resp.StatusCode)
	}
}

// AC: the over-receipt answer renders through the dedicated renderer — 409,
// code exactly "grn_over_receipt" (stable for equality matching), error
// carrying the full pinned detail line — while the other codes on this route
// keep the shared mapping, and a malformed body is a 400 before any call.
func TestGoodsReceiptRouteErrors(t *testing.T) {
	detail := "grn_over_receipt: po_item=11111111-0000-0000-0000-000000000001 received=1 requested=2 ordered=2"
	for name, tc := range map[string]struct {
		err       error
		want      int
		wantCode  string
		wantError string
	}{
		"over receipt": {status.Error(codes.Aborted, detail), http.StatusConflict, "grn_over_receipt", detail},
		"illegal transition": {status.Error(codes.Aborted, "po_illegal_transition"), http.StatusConflict,
			"po_illegal_transition", "goods can only be received on an acknowledged or partially received purchase order"},
		"po 404":      {status.Error(codes.NotFound, "po_not_found"), http.StatusNotFound, "po_not_found", "purchase order not found"},
		"bad po id":   {status.Error(codes.InvalidArgument, "po_id_invalid"), http.StatusBadRequest, "po_id_invalid", "purchase order id must be a UUID"},
		"empty items": {status.Error(codes.InvalidArgument, "grn_items_empty"), http.StatusBadRequest, "grn_items_empty", "a goods receipt needs at least one item line"},
	} {
		fake := &fakePOClient{
			fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
			grnErr:                tc.err,
		}
		app, verifier, priv, kid := newTestApp(t)
		registerPurchaseOrderRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleBuyer)

		resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/goods-receipts", token,
			`{"items":[{"po_item_id":"po-item-1","qty_received":1}]}`, nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != tc.wantCode {
			t.Fatalf("%s: code = %v, want %q", name, body["code"], tc.wantCode)
		}
		if body["error"] != tc.wantError {
			t.Fatalf("%s: error = %v, want %q", name, body["error"], tc.wantError)
		}
	}

	// Malformed JSON: 400 before any procurement call.
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)
	resp, body := reqDo(t, app, "POST", "/api/v1/purchase-orders/po-9/goods-receipts",
		reqTokenFor(t, priv, kid, rbac.RoleBuyer), `{not json`, nil)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "body_invalid" {
		t.Fatalf("malformed body: (%d, %v)", resp.StatusCode, body)
	}
	if fake.grnReq != nil {
		t.Fatalf("procurement called on a malformed body")
	}
}

// The #38 read methods on the PO fake: recording like the conversion/lifecycle
// arms above.
func (f *fakePOClient) ListPurchaseOrders(ctx context.Context, in *procurementv1.ListPurchaseOrdersRequest, opts ...grpc.CallOption) (*procurementv1.ListPurchaseOrdersResponse, error) {
	f.listCtx, f.listReq = ctx, in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRes, nil
}

func (f *fakePOClient) GetPurchaseOrder(ctx context.Context, in *procurementv1.GetPurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	f.getCtx, f.getReq = ctx, in
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRes, nil
}

// AC (#38): the read routes are wired through server.New with
// WithProcurement behind RequirePermission(rbac.PORead) — the broad read
// permission a viewer holds. The list forwards the query params and renders
// the {purchase_orders, total, page, page_size} envelope with header-only
// rows; the detail returns the aggregate with item ids; the claims ride
// along as gRPC metadata.
func TestPurchaseOrderReadRoutesThroughServerNew(t *testing.T) {
	fake := &fakePOClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		listRes: &procurementv1.ListPurchaseOrdersResponse{
			PurchaseOrders: []*procurementv1.PurchaseOrder{
				{Id: "po-2", TenantId: "tenant-29", Number: "PO-000002", Status: "issued", Currency: "IDR", TotalCents: 1000},
				{Id: "po-1", TenantId: "tenant-29", Number: "PO-000001", Status: "draft", Currency: "IDR"},
			},
			Total: 42, Page: 2, PageSize: 2,
		},
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })
	token := reqTokenFor(t, priv, kid, rbac.RoleViewer) // viewer holds po:read

	resp, body := reqDo(t, app, "GET", "/api/v1/purchase-orders?q=po-0001&status=draft&page=2&page_size=2", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body = %v", resp.StatusCode, body)
	}
	if body["total"] != float64(42) || body["page"] != float64(2) || body["page_size"] != float64(2) {
		t.Fatalf("envelope = %v", body)
	}
	rows, _ := body["purchase_orders"].([]any)
	if len(rows) != 2 {
		t.Fatalf("purchase_orders = %v", body["purchase_orders"])
	}
	first, _ := rows[0].(map[string]any)
	if first["number"] != "PO-000002" || first["status"] != "issued" {
		t.Fatalf("row JSON = %v", first)
	}
	if items, _ := first["items"].([]any); len(items) != 0 {
		t.Fatalf("list row carries items: %v", first["items"])
	}
	// Query params forwarded; claims rode along as metadata.
	if fake.listReq.GetQuery() != "po-0001" || fake.listReq.GetStatus() != "draft" ||
		fake.listReq.GetPage() != 2 || fake.listReq.GetPageSize() != 2 {
		t.Fatalf("forwarded list = %+v", fake.listReq)
	}
	md := metadataOf(t, fake.listCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("propagated metadata = %v", md)
	}

	// Detail: the aggregate with item ids (receipt lines reference them).
	detail := samplePO()
	detail.Items[0].Id = "po-item-1"
	fake.getRes = detail
	resp, body = reqDo(t, app, "GET", "/api/v1/purchase-orders/po-1", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, body = %v", resp.StatusCode, body)
	}
	if body["number"] != "PO-000001" || body["status"] != "draft" || body["requisition_id"] != "pr-1" {
		t.Fatalf("detail body = %v", body)
	}
	items, _ := body["items"].([]any)
	item0, _ := items[0].(map[string]any)
	if item0["id"] != "po-item-1" || item0["qty_ordered"] != float64(2) {
		t.Fatalf("detail items = %v", items)
	}
	if fake.getReq.GetId() != "po-1" {
		t.Fatalf("forwarded get = %+v", fake.getReq)
	}
	md = metadataOf(t, fake.getCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("get metadata = %v", md)
	}
}

// Anonymous access: both read routes answer 401 before any procurement call
// (the bearerAuth middleware runs before the permission gate).
func TestPurchaseOrderReadRoutesRequireBearer(t *testing.T) {
	fake := &fakePOClient{fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}}
	app, verifier, _, _ := newTestApp(t)
	registerPurchaseOrderRoutes(app, verifier, fake)

	for _, path := range []string{"/api/v1/purchase-orders", "/api/v1/purchase-orders/po-1"} {
		resp, body := reqDo(t, app, "GET", path, "", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: status = %d, body = %v, want 401", path, resp.StatusCode, body)
		}
	}
	if fake.listReq != nil || fake.getReq != nil {
		t.Fatal("procurement was called without a bearer token")
	}
}

// The read routes' error slice of the shared mapping: the #38 filter codes
// answer 400 with machine code + human sentence, missing/cross-tenant
// answers 404 po_not_found.
func TestPurchaseOrderReadRouteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"bad status":   {status.Error(codes.InvalidArgument, "po_status_invalid"), http.StatusBadRequest},
		"bad page":     {status.Error(codes.InvalidArgument, "po_page_invalid"), http.StatusBadRequest},
		"po not found": {status.Error(codes.NotFound, "po_not_found"), http.StatusNotFound},
	} {
		fake := &fakePOClient{
			fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
			listErr:               tc.err,
			getErr:                tc.err,
		}
		app, verifier, priv, kid := newTestApp(t)
		registerPurchaseOrderRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleViewer)

		resp, body := reqDo(t, app, "GET", "/api/v1/purchase-orders?status=paused", token, "", nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: list status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: list body %v lost the machine code", name, body)
		}
		if human, ok := body["error"].(string); !ok || strings.TrimSpace(human) == "" {
			t.Fatalf("%s: list body %v lost the human sentence", name, body)
		}

		resp, body = reqDo(t, app, "GET", "/api/v1/purchase-orders/po-9", token, "", nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: detail status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: detail body %v lost the machine code", name, body)
		}
	}
}
