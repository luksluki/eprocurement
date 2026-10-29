package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// fakeRequisitionClient satisfies ProcurementClient (vendor methods via the
// embedded vendor fake) and records what the gateway forwarded.
type fakeRequisitionClient struct {
	*fakeVendorClient
	lastCtx   context.Context
	createReq *procurementv1.CreateRequisitionRequest
	updateReq *procurementv1.UpdateRequisitionRequest
	submitReq *procurementv1.SubmitRequisitionRequest
	got       *procurementv1.Requisition
	reqErr    error

	getReq *procurementv1.GetRequisitionRequest
	getRes *procurementv1.Requisition
	getErr error
	getCtx context.Context

	listReq *procurementv1.ListRequisitionsRequest
	listRes *procurementv1.ListRequisitionsResponse
	listErr error
	listCtx context.Context
}

func (f *fakeRequisitionClient) CreateRequisition(ctx context.Context, in *procurementv1.CreateRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error) {
	f.lastCtx, f.createReq = ctx, in
	return f.reply()
}

func (f *fakeRequisitionClient) UpdateRequisition(ctx context.Context, in *procurementv1.UpdateRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error) {
	f.lastCtx, f.updateReq = ctx, in
	return f.reply()
}

func (f *fakeRequisitionClient) SubmitRequisition(ctx context.Context, in *procurementv1.SubmitRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error) {
	f.lastCtx, f.submitReq = ctx, in
	return f.reply()
}

func (f *fakeRequisitionClient) GetRequisition(ctx context.Context, in *procurementv1.GetRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error) {
	f.getCtx, f.getReq = ctx, in
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRes, nil
}

func (f *fakeRequisitionClient) ListRequisitions(ctx context.Context, in *procurementv1.ListRequisitionsRequest, opts ...grpc.CallOption) (*procurementv1.ListRequisitionsResponse, error) {
	f.listCtx, f.listReq = ctx, in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRes, nil
}

func (f *fakeRequisitionClient) reply() (*procurementv1.Requisition, error) {
	if f.reqErr != nil {
		return nil, f.reqErr
	}
	return f.got, nil
}

// The #30 approval methods satisfy the widened ProcurementClient interface;
// the requisition tests never call them (the approval tests use their own
// fake with recording).
func (f *fakeRequisitionClient) DecideApproval(ctx context.Context, in *procurementv1.DecideApprovalRequest, opts ...grpc.CallOption) (*procurementv1.DecideApprovalResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

// The #35 purchase-order method and the #36 lifecycle methods satisfy the
// widened ProcurementClient interface; the PO tests use their own fake with
// recording.
func (f *fakeRequisitionClient) CreatePurchaseOrder(ctx context.Context, in *procurementv1.CreatePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.CreatePurchaseOrderResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

// The #38 PO read methods satisfy the widened ProcurementClient interface;
// the PO tests use their own fake with recording.
func (f *fakeRequisitionClient) ListPurchaseOrders(ctx context.Context, in *procurementv1.ListPurchaseOrdersRequest, opts ...grpc.CallOption) (*procurementv1.ListPurchaseOrdersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) GetPurchaseOrder(ctx context.Context, in *procurementv1.GetPurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) IssuePurchaseOrder(ctx context.Context, in *procurementv1.IssuePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) AcknowledgePurchaseOrder(ctx context.Context, in *procurementv1.AcknowledgePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) CancelPurchaseOrder(ctx context.Context, in *procurementv1.CancelPurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) ClosePurchaseOrder(ctx context.Context, in *procurementv1.ClosePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

// The #37 goods-receipt method satisfies the widened ProcurementClient
// interface; the goods-receipt tests use their own fake with recording.
func (f *fakeRequisitionClient) PostGoodsReceipt(ctx context.Context, in *procurementv1.PostGoodsReceiptRequest, opts ...grpc.CallOption) (*procurementv1.PostGoodsReceiptResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

// The #39 invoice methods satisfy the widened ProcurementClient interface;
// the invoice tests use their own fake with recording.
func (f *fakeRequisitionClient) CreateInvoice(ctx context.Context, in *procurementv1.CreateInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) GetInvoice(ctx context.Context, in *procurementv1.GetInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

// The #40 match methods satisfy the widened ProcurementClient interface;
// the match/settings tests use their own fakes with recording.
func (f *fakeRequisitionClient) MatchInvoice(ctx context.Context, in *procurementv1.MatchInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) GetMatchSettings(ctx context.Context, in *procurementv1.GetMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) ReplaceMatchSettings(ctx context.Context, in *procurementv1.ReplaceMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	return nil, status.Error(codes.Unimplemented, "not called in requisition tests")
}

func (f *fakeRequisitionClient) GetApprovalRules(ctx context.Context, in *procurementv1.GetApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error) {
	return &procurementv1.ApprovalRulesResponse{}, nil
}

func (f *fakeRequisitionClient) ReplaceApprovalRules(ctx context.Context, in *procurementv1.ReplaceApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error) {
	return &procurementv1.ApprovalRulesResponse{}, nil
}

// reqTokenFor mints a requisition-test token (user-29 of tenant-29) with the
// given roles.
func reqTokenFor(t *testing.T, priv ed25519.PrivateKey, kid string, roles ...string) string {
	t.Helper()
	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Iss: "identity.eprocurement", Sub: "user-29", TenantID: "tenant-29",
		Roles: roles, IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func reqDo(t *testing.T, app *fiber.App, method, path, token, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func sampleRequisition() *procurementv1.Requisition {
	return &procurementv1.Requisition{
		Id: "pr-1", TenantId: "tenant-29", Number: "PR-000001", RequesterId: "user-29",
		Status: "draft", Currency: "IDR", TotalCents: 15000000,
		Items: []*procurementv1.RequisitionItem{
			{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
		},
		CreatedAt: "2026-10-23T10:00:00Z", CreatedBy: "user-29",
		UpdatedAt: "2026-10-23T10:00:00Z", UpdatedBy: "user-29",
	}
}

// AC: the routes are wired through server.New with WithProcurement — a
// registration that only exists behind the option must not 404 in the real
// binary wiring. Happy path: currency normalized at the edge, claims
// propagated, server-computed number and total echoed back.
func TestRequisitionRoutesThroughServerNew(t *testing.T) {
	fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, got: sampleRequisition()}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })

	resp, body := reqDo(t, app, "POST", "/api/v1/requisitions",
		reqTokenFor(t, priv, kid, rbac.RoleRequester), `{"currency":" idr ","items":[{"description":" Laptop ","qty":2,"unit_price_cents":7500000}]}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", resp.StatusCode, body)
	}
	if body["number"] != "PR-000001" || body["total_cents"] != float64(15000000) || body["status"] != "draft" {
		t.Fatalf("create body = %v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}

	// The gateway normalized the currency and trimmed item strings.
	if fake.createReq.GetCurrency() != "IDR" {
		t.Fatalf("forwarded currency = %q, want IDR", fake.createReq.GetCurrency())
	}
	if len(fake.createReq.GetItems()) != 1 ||
		fake.createReq.GetItems()[0].GetDescription() != "Laptop" {
		t.Fatalf("forwarded items = %+v", fake.createReq.GetItems())
	}
	// The verified claims were propagated as gRPC metadata.
	md := metadataOf(t, fake.lastCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" || md["x-role"] != rbac.RoleRequester {
		t.Fatalf("propagated metadata = %v", md)
	}
}

func metadataOf(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("no outgoing metadata on the procurement call")
	}
	out := map[string]string{}
	for _, key := range []string{rbac.MDTenantID, rbac.MDUserID, rbac.MDRole, "x-trace-id"} {
		if got := md.Get(key); len(got) > 0 {
			out[key] = got[0]
		}
	}
	return out
}

// Viewer holds no requisition:write: all three endpoints are 403 before any
// procurement call.
func TestRequisitionRoutesRequirePermission(t *testing.T) {
	fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, nil)

	token := reqTokenFor(t, priv, kid, rbac.RoleViewer)
	for _, method := range []string{
		"POST /api/v1/requisitions",
		"PUT /api/v1/requisitions/pr-1",
		"POST /api/v1/requisitions/pr-1/submit",
	} {
		parts := strings.SplitN(method, " ", 2)
		resp, _ := reqDo(t, app, parts[0], parts[1], token, `{}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as viewer: status = %d, want 403", method, resp.StatusCode)
		}
	}
	if fake.createReq != nil || fake.updateReq != nil || fake.submitReq != nil {
		t.Fatal("procurement was called despite denied permission")
	}
}

// PUT replaces the basket and forwards the id; submit forwards the
// Idempotency-Key header as the gRPC idempotency_key field.
func TestRequisitionRoutesForwarding(t *testing.T) {
	fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, got: sampleRequisition()}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, nil)
	token := reqTokenFor(t, priv, kid, rbac.RoleRequester)

	resp, _ := reqDo(t, app, "PUT", "/api/v1/requisitions/pr-1", token,
		`{"currency":"IDR","items":[{"description":"Mouse","qty":3,"unit_price_cents":150000}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	if fake.updateReq.GetId() != "pr-1" || len(fake.updateReq.GetItems()) != 1 ||
		fake.updateReq.GetItems()[0].GetQty() != 3 {
		t.Fatalf("forwarded update = %+v", fake.updateReq)
	}

	resp, _ = reqDo(t, app, "POST", "/api/v1/requisitions/pr-1/submit", token, "", map[string]string{
		"Idempotency-Key": "  key-29-a  ",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d", resp.StatusCode)
	}
	if fake.submitReq.GetId() != "pr-1" || fake.submitReq.GetIdempotencyKey() != "key-29-a" {
		t.Fatalf("forwarded submit = %+v", fake.submitReq)
	}

	// The header is optional but bounded: >64 chars after trimming is 400
	// without a procurement call.
	fake.submitReq = nil
	resp, body := reqDo(t, app, "POST", "/api/v1/requisitions/pr-1/submit", token, "", map[string]string{
		"Idempotency-Key": strings.Repeat("k", 65),
	})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "requisition_idempotency_key_invalid" {
		t.Fatalf("long key = %d %v, want 400 requisition_idempotency_key_invalid", resp.StatusCode, body)
	}
	if fake.submitReq != nil {
		t.Fatal("procurement was called despite invalid key length")
	}
}

// The requisition slice of the shared gRPC→HTTP mapping: illegal transition
// (Aborted) and key conflict (AlreadyExists) are 409, ownership denial is
// 403, cross-tenant/missing is 404 — machine codes always preserved.
func TestRequisitionRoutesErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"illegal transition": {status.Error(codes.Aborted, "requisition_illegal_transition"), http.StatusConflict},
		"key conflict":       {status.Error(codes.AlreadyExists, "requisition_idempotency_key_conflict"), http.StatusConflict},
		"forbidden":          {status.Error(codes.PermissionDenied, "requisition_forbidden"), http.StatusForbidden},
		"not found":          {status.Error(codes.NotFound, "requisition_not_found"), http.StatusNotFound},
		"bad items":          {status.Error(codes.InvalidArgument, "requisition_qty_invalid"), http.StatusBadRequest},
	} {
		fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, reqErr: tc.err}
		app, verifier, priv, kid := newTestApp(t)
		registerRequisitionRoutes(app, verifier, fake, nil)
		token := reqTokenFor(t, priv, kid, rbac.RoleRequester)

		resp, body := reqDo(t, app, "POST", "/api/v1/requisitions", token, `{"currency":"IDR","items":[{"description":"x","qty":1}]}`, nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the machine code", name, body)
		}
	}
}

// The list route (#32): query params forwarded, {requisitions, total, page,
// page_size} envelope, claims propagated as metadata, requisition:read broad
// enough for a viewer, and the shared 400 mapping for bad status/page.
func TestRequisitionListRoute(t *testing.T) {
	fake := &fakeRequisitionClient{
		fakeVendorClient: &fakeVendorClient{},
		listRes: &procurementv1.ListRequisitionsResponse{
			Requisitions: []*procurementv1.Requisition{
				{Id: "pr-2", TenantId: "tenant-29", Number: "PR-000002", Status: "submitted", Currency: "IDR", TotalCents: 1000},
				{Id: "pr-1", TenantId: "tenant-29", Number: "PR-000001", Status: "draft", Currency: "IDR"},
			},
			Total: 42, Page: 2, PageSize: 2,
		},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, nil)
	token := reqTokenFor(t, priv, kid, rbac.RoleViewer) // viewer holds requisition:read

	resp, body := reqDo(t, app, "GET", "/api/v1/requisitions?q=pr-0001&status=draft&page=2&page_size=2", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body = %v", resp.StatusCode, body)
	}
	if body["total"] != float64(42) || body["page"] != float64(2) || body["page_size"] != float64(2) {
		t.Fatalf("envelope = %v", body)
	}
	rows, _ := body["requisitions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("requisitions = %v", body["requisitions"])
	}
	first, _ := rows[0].(map[string]any)
	if first["number"] != "PR-000002" || first["status"] != "submitted" {
		t.Fatalf("row JSON = %v", first)
	}

	// Query params forwarded to procurement; claims rode along as metadata.
	if fake.listReq.GetQuery() != "pr-0001" || fake.listReq.GetStatus() != "draft" ||
		fake.listReq.GetPage() != 2 || fake.listReq.GetPageSize() != 2 {
		t.Fatalf("forwarded request = %+v", fake.listReq)
	}
	md := metadataOf(t, fake.listCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("propagated metadata = %v", md)
	}

	// Invalid status/page from procurement map to 400 with the machine code.
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"bad status": {status.Error(codes.InvalidArgument, "requisition_status_invalid"), "requisition_status_invalid"},
		"bad page":   {status.Error(codes.InvalidArgument, "requisition_page_invalid"), "requisition_page_invalid"},
	} {
		fake.listErr = tc.err
		resp, body := reqDo(t, app, "GET", "/api/v1/requisitions?status=paused", token, "", nil)
		if resp.StatusCode != http.StatusBadRequest || body["code"] != tc.code {
			t.Fatalf("%s: got (%d, %v), want (400, %s)", name, resp.StatusCode, body, tc.code)
		}
	}
}

// The detail route (#33): GET /api/v1/requisitions/:id returns the bodies
// the list never carries and embeds attachment metadata from documents
// (vendor-detail parity). The wizard's draft restore reads through it.
func TestRequisitionDetailRouteEmbedsAttachments(t *testing.T) {
	fake := &fakeRequisitionClient{
		fakeVendorClient: &fakeVendorClient{},
		getRes: func() *procurementv1.Requisition {
			r := sampleRequisition()
			r.Items = []*procurementv1.RequisitionItem{
				{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000},
			}
			return r
		}(),
	}
	documents := &fakeDocumentsClient{listResp: &documentsv1.ListAttachmentsResponse{
		Attachments: []*documentsv1.Attachment{
			{Id: "att-1", EntityType: "requisition", EntityId: "pr-1", Filename: "quote.pdf", Status: "ready"},
		},
	}}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, documents)
	token := reqTokenFor(t, priv, kid, rbac.RoleRequester) // viewer would read too

	resp, body := reqDo(t, app, "GET", "/api/v1/requisitions/pr-1", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, body = %v", resp.StatusCode, body)
	}
	if body["number"] != "PR-000001" || body["status"] != "draft" {
		t.Fatalf("detail body = %v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items not carried: %v", body["items"])
	}
	attachments, _ := body["attachments"].([]any)
	if len(attachments) != 1 || attachments[0].(map[string]any)["filename"] != "quote.pdf" {
		t.Fatalf("attachments = %v", body["attachments"])
	}
	if fake.getReq.GetId() != "pr-1" {
		t.Fatalf("forwarded get = %+v", fake.getReq)
	}
	if documents.listReq == nil || documents.listReq.GetEntityType() != "requisition" || documents.listReq.GetEntityId() != "pr-1" {
		t.Fatalf("documents list request = %+v", documents.listReq)
	}
	md := metadataOf(t, fake.getCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("propagated metadata = %v", md)
	}
}

// Degradation parity with the vendor routes: without a documents client the
// detail still serves the requisition, just without attachments (the
// registerRequisitionRoutes nil guard — an embed without the guard would
// panic here).
func TestRequisitionDetailWithoutDocuments(t *testing.T) {
	fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, getRes: sampleRequisition()}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, nil)
	token := reqTokenFor(t, priv, kid, rbac.RoleViewer)

	resp, body := reqDo(t, app, "GET", "/api/v1/requisitions/pr-1", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, body = %v", resp.StatusCode, body)
	}
	if _, has := body["attachments"]; has {
		t.Fatalf("attachments key should be absent without documents: %v", body)
	}
}

// Detail error mapping and cross-tenant invisibility ride procurement's
// codes; requisition:read is broad (a viewer may read the detail).
func TestRequisitionDetailErrors(t *testing.T) {
	fake := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, getErr: status.Error(codes.NotFound, "requisition_not_found")}
	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, fake, nil)
	token := reqTokenFor(t, priv, kid, rbac.RoleViewer)

	resp, body := reqDo(t, app, "GET", "/api/v1/requisitions/pr-other-tenant", token, "", nil)
	if resp.StatusCode != http.StatusNotFound || body["code"] != "requisition_not_found" {
		t.Fatalf("missing/cross-tenant = (%d, %v), want 404 requisition_not_found", resp.StatusCode, body)
	}
}
