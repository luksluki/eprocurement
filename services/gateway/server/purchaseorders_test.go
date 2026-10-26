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
// gateway forwarded to the conversion call.
type fakePOClient struct {
	*fakeRequisitionClient

	poReq *procurementv1.CreatePurchaseOrderRequest
	poRes *procurementv1.CreatePurchaseOrderResponse
	poErr error
	poCtx context.Context
}

func (f *fakePOClient) CreatePurchaseOrder(ctx context.Context, in *procurementv1.CreatePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.CreatePurchaseOrderResponse, error) {
	f.poCtx, f.poReq = ctx, in
	if f.poErr != nil {
		return nil, f.poErr
	}
	return f.poRes, nil
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
