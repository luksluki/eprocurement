package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// fakeInvoiceClient satisfies ProcurementClient (vendor + requisition via
// the embedded fakes) and records what the gateway forwarded to the invoice
// calls.
type fakeInvoiceClient struct {
	*fakeRequisitionClient

	createReq *procurementv1.CreateInvoiceRequest
	createRes *procurementv1.Invoice
	createErr error
	createCtx context.Context

	getReq *procurementv1.GetInvoiceRequest
	getRes *procurementv1.Invoice
	getErr error
	getCtx context.Context

	listReq *procurementv1.ListInvoicesRequest
	listRes *procurementv1.ListInvoicesResponse
	listErr error
	listCtx context.Context

	matchReq *procurementv1.MatchInvoiceRequest
	matchRes *procurementv1.Invoice
	matchErr error
	matchCtx context.Context
}

func (f *fakeInvoiceClient) ListInvoices(ctx context.Context, in *procurementv1.ListInvoicesRequest, opts ...grpc.CallOption) (*procurementv1.ListInvoicesResponse, error) {
	f.listCtx, f.listReq = ctx, in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRes, nil
}

func (f *fakeInvoiceClient) MatchInvoice(ctx context.Context, in *procurementv1.MatchInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	f.matchCtx, f.matchReq = ctx, in
	if f.matchErr != nil {
		return nil, f.matchErr
	}
	return f.matchRes, nil
}

// The settings surface uses its own recording fake (matchsettings_test.go);
// these stubs only satisfy the widened interface for this suite's app.
func (f *fakeInvoiceClient) GetMatchSettings(ctx context.Context, in *procurementv1.GetMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	return nil, status.Error(codes.Unimplemented, "not called in invoice tests")
}

func (f *fakeInvoiceClient) ReplaceMatchSettings(ctx context.Context, in *procurementv1.ReplaceMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	return nil, status.Error(codes.Unimplemented, "not called in invoice tests")
}

func (f *fakeInvoiceClient) CreateInvoice(ctx context.Context, in *procurementv1.CreateInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	f.createCtx, f.createReq = ctx, in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createRes, nil
}

func (f *fakeInvoiceClient) GetInvoice(ctx context.Context, in *procurementv1.GetInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	f.getCtx, f.getReq = ctx, in
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRes, nil
}

func sampleInvoice() *procurementv1.Invoice {
	return &procurementv1.Invoice{
		Id: "inv-1", TenantId: "tenant-29", PoId: "po-1", VendorId: "v-1",
		Number: "INV/2026/0001", Currency: "IDR", TotalCents: 15000000,
		Status: "pending",
		Items: []*procurementv1.InvoiceItem{
			{PoItemId: "po-item-1", Qty: 2, UnitPriceCents: 7500000},
		},
		CreatedBy: "user-29", CreatedAt: "2026-10-27T10:00:00Z",
	}
}

// invoiceTestApp wires the invoice routes (registerInvoiceRoutes) against
// fakes and returns a finance header plus a signer for tokens with other
// roles.
func invoiceTestApp(t *testing.T, documents DocumentsClient) (*fiber.App, *fakeInvoiceClient, http.Header, func(roles ...string) string) {
	t.Helper()
	fake := &fakeInvoiceClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		createRes:             sampleInvoice(),
		getRes:                sampleInvoice(),
	}
	app, verifier, priv, kid := newTestApp(t)
	registerInvoiceRoutes(app, verifier, fake, documents)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-29", TenantID: "tenant-29", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleFinance))
	header.Set("Content-Type", "application/json")
	return app, fake, header, sign
}

// AC: the create route is wired through server.New with WithProcurement +
// WithDocuments, invoice:write (finance) grants access, the trimmed po_id,
// number, and item ids are forwarded, claims ride along as gRPC metadata,
// and the 201 body carries the stored invoice — the detail read embedding
// attachment metadata when documents is wired.
func TestInvoiceRouteThroughServerNew(t *testing.T) {
	fake := &fakeInvoiceClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		createRes:             sampleInvoice(),
		getRes:                sampleInvoice(),
	}
	documents := &fakeDocumentsClient{
		listResp: &documentsv1.ListAttachmentsResponse{
			Attachments: []*documentsv1.Attachment{{
				Id: "att-1", TenantId: "tenant-29", EntityType: "invoice", EntityId: "inv-1",
				Filename: "scan.pdf", MimeType: "application/pdf", SizeBytes: 12, Status: "ready",
			}},
		},
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier,
		WithProcurement(fake), WithDocuments(documents))
	t.Cleanup(func() { _ = app.Shutdown() })
	token := reqTokenFor(t, priv, kid, rbac.RoleFinance)

	body := `{"po_id":"  po-1  ","number":"  INV/2026/0001  ","items":[{"po_item_id":"  po-item-1  ","qty":2,"unit_price_cents":7500000}]}`
	resp, parsed := reqDo(t, app, "POST", "/api/v1/invoices", token, body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", resp.StatusCode, parsed)
	}
	if parsed["number"] != "INV/2026/0001" || parsed["po_id"] != "po-1" ||
		parsed["vendor_id"] != "v-1" || parsed["currency"] != "IDR" ||
		parsed["total_cents"] != float64(15000000) || parsed["status"] != "pending" ||
		parsed["match_result_json"] != "" {
		t.Fatalf("invoice body = %v", parsed)
	}
	items, _ := parsed["items"].([]any)
	first, _ := items[0].(map[string]any)
	if first["po_item_id"] != "po-item-1" || first["qty"] != float64(2) ||
		first["unit_price_cents"] != float64(7500000) {
		t.Fatalf("items = %v", parsed["items"])
	}
	// The trimmed fields were forwarded.
	if fake.createReq.GetPoId() != "po-1" || fake.createReq.GetNumber() != "INV/2026/0001" ||
		len(fake.createReq.GetItems()) != 1 || fake.createReq.GetItems()[0].GetPoItemId() != "po-item-1" {
		t.Fatalf("forwarded request = %+v", fake.createReq)
	}
	// The verified claims were propagated as gRPC metadata.
	md := metadataOf(t, fake.createCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("metadata = %v", md)
	}

	// The detail read embeds attachment metadata (vendor-detail parity).
	resp, parsed = reqDo(t, app, "GET", "/api/v1/invoices/inv-1",
		reqTokenFor(t, priv, kid, rbac.RoleViewer), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body = %v", resp.StatusCode, parsed)
	}
	attachments, _ := parsed["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %v", parsed["attachments"])
	}
	att, _ := attachments[0].(map[string]any)
	if att["entity_type"] != "invoice" || att["entity_id"] != "inv-1" {
		t.Fatalf("attachment = %v", att)
	}
	if documents.listReq.GetEntityType() != "invoice" || documents.listReq.GetEntityId() != "inv-1" {
		t.Fatalf("documents list request = %+v", documents.listReq)
	}
}

// AC: without a documents client the detail read still works and the
// attachments key is absent (vendors.go parity).
func TestInvoiceRouteDetailWithoutDocuments(t *testing.T) {
	app, fake, header, _ := invoiceTestApp(t, nil)
	resp, parsed := reqDo(t, app, http.MethodGet, "/api/v1/invoices/inv-1", tokenOf(header), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body = %v", resp.StatusCode, parsed)
	}
	if _, present := parsed["attachments"]; present {
		t.Fatalf("attachments key present without documents: %v", parsed)
	}
	if fake.getReq.GetId() != "inv-1" {
		t.Fatalf("forwarded get = %+v", fake.getReq)
	}
}

// AC: invoice:write is finance/admin — every other role is rejected at the
// gateway before any procurement call (nothing forwarded), and the implicit
// admin pass-through holds.
func TestInvoiceRouteRequiresPermission(t *testing.T) {
	for name, role := range map[string]string{
		"requester": rbac.RoleRequester,
		"approver":  rbac.RoleApprover,
		"buyer":     rbac.RoleBuyer,
		"viewer":    rbac.RoleViewer,
	} {
		app, fake, _, sign := invoiceTestApp(t, nil)
		resp, body := reqDo(t, app, "POST", "/api/v1/invoices", sign(role),
			`{"po_id":"po-1","number":"INV-1","items":[{"po_item_id":"po-item-1","qty":1,"unit_price_cents":1}]}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["required"] != rbac.InvoiceWrite {
			t.Fatalf("%s: body = %v, want required %s", name, body, rbac.InvoiceWrite)
		}
		if fake.createReq != nil {
			t.Fatalf("%s: procurement called despite the denial", name)
		}
	}
	// Admin passes implicitly (admin holds every permission).
	app, fake, _, sign := invoiceTestApp(t, nil)
	resp, _ := reqDo(t, app, "POST", "/api/v1/invoices", sign(rbac.RoleAdmin),
		`{"po_id":"po-1","number":"INV-1","items":[{"po_item_id":"po-item-1","qty":1,"unit_price_cents":1}]}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin: status = %d, want 201", resp.StatusCode)
	}
	if fake.createReq == nil {
		t.Fatalf("admin: procurement not called")
	}
}

// AC: the over-bill answer renders through the dedicated renderer — 409,
// code exactly "inv_over_bill" (stable for equality matching), error
// carrying the full pinned detail line — while the other codes on these
// routes keep the shared mapping, and a malformed body is a 400 before any
// call.
func TestInvoiceRouteErrors(t *testing.T) {
	detail := "inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000"
	for name, tc := range map[string]struct {
		err       error
		want      int
		wantCode  string
		wantError string
	}{
		"over bill": {status.Error(codes.Aborted, detail), http.StatusConflict, "inv_over_bill", detail},
		"illegal transition": {status.Error(codes.Aborted, "po_illegal_transition"), http.StatusConflict,
			"po_illegal_transition", "only acknowledged, partially received, or received purchase orders can be invoiced"},
		"duplicate number": {status.Error(codes.AlreadyExists, "inv_number_duplicate"), http.StatusConflict,
			"inv_number_duplicate", "this vendor already has an invoice with this number"},
		"po 404":    {status.Error(codes.NotFound, "po_not_found"), http.StatusNotFound, "po_not_found", "purchase order not found"},
		"inv 404":   {status.Error(codes.NotFound, "inv_not_found"), http.StatusNotFound, "inv_not_found", "invoice not found"},
		"bad po id": {status.Error(codes.InvalidArgument, "po_id_invalid"), http.StatusBadRequest, "po_id_invalid", "purchase order id must be a UUID"},
		"empty items": {status.Error(codes.InvalidArgument, "inv_items_empty"), http.StatusBadRequest,
			"inv_items_empty", "an invoice needs at least one item line"},
	} {
		app, fake, header, _ := invoiceTestApp(t, nil)
		fake.createErr = tc.err
		resp, body := reqDo(t, app, "POST", "/api/v1/invoices", tokenOf(header),
			`{"po_id":"po-9","number":"INV-9","items":[{"po_item_id":"po-item-1","qty":1,"unit_price_cents":1}]}`, nil)
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
	app, fake, header, _ := invoiceTestApp(t, nil)
	resp, body := reqDo(t, app, "POST", "/api/v1/invoices", tokenOf(header), `{not json`, nil)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "body_invalid" {
		t.Fatalf("malformed body: (%d, %v)", resp.StatusCode, body)
	}
	if fake.createReq != nil {
		t.Fatalf("procurement called on a malformed body")
	}
}

// tokenOf extracts the bearer header value the helper apps were built with.
func tokenOf(header http.Header) string { return header.Get("Authorization")[len("Bearer "):] }

// AC (#39 attachments): declare checks the invoice exists in procurement
// first (the vendor pattern) — an unknown invoice answers 404 before any
// documents call — and the declaration carries entity type "invoice" with
// the path id. Complete/download bind to the invoice area's permissions.
func TestInvoiceAttachmentRoutes(t *testing.T) {
	documents := &fakeDocumentsClient{
		createResp: &documentsv1.CreateUploadResponse{
			Attachment: &documentsv1.Attachment{
				Id: "att-1", TenantId: "tenant-29", EntityType: "invoice", EntityId: "inv-1",
				Filename: "scan.pdf", MimeType: "application/pdf", SizeBytes: 12, Status: "pending",
			},
			UploadUrl: "http://minio:9000/attachments", Method: "PUT", ExpiresAt: "2026-10-27T10:15:00Z",
		},
	}
	app, fake, header, sign := invoiceTestApp(t, documents)

	// Declare: finance passes, the invoice existence is checked first, and
	// the documents call carries the invoice entity.
	body := `{"filename":"scan.pdf","mime_type":"application/pdf","size_bytes":12,"checksum_sha256":"` +
		strings.Repeat("ab", 32) + `"}`
	resp := doReq(t, app, header, http.MethodPost, "/api/v1/invoices/inv-1/attachments", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("declare status = %d", resp.StatusCode)
	}
	if fake.getReq == nil || fake.getReq.GetId() != "inv-1" {
		t.Fatalf("existence check = %+v", fake.getReq)
	}
	if documents.createReq.GetEntityType() != "invoice" || documents.createReq.GetEntityId() != "inv-1" {
		t.Fatalf("documents create = %+v", documents.createReq)
	}

	// Unknown invoice: procurement's 404 answers before any documents call.
	documents.createReq = nil
	fake.getErr = status.Error(codes.NotFound, "inv_not_found")
	resp = doReq(t, app, header, http.MethodPost, "/api/v1/invoices/nope/attachments", body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown invoice declare status = %d, want 404", resp.StatusCode)
	}
	if documents.createReq != nil {
		t.Fatalf("documents called despite the unknown invoice")
	}

	// invoice:write is finance/admin: a buyer cannot declare.
	fake.getErr = nil
	buyerHeader := http.Header{}
	buyerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleBuyer))
	buyerHeader.Set("Content-Type", "application/json")
	resp = doReq(t, app, buyerHeader, http.MethodPost, "/api/v1/invoices/inv-1/attachments", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("buyer declare status = %d, want 403", resp.StatusCode)
	}

	// Download is invoice:read — a viewer may fetch the URL.
	documents.downloadResp = &documentsv1.GetDownloadUrlResponse{
		Attachment:  documents.createResp.GetAttachment(),
		DownloadUrl: "http://minio:9000/attachments?X-Amz-Signature=sig",
		ExpiresAt:   "2026-10-27T10:15:00Z",
	}
	viewerHeader := http.Header{}
	viewerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleViewer))
	resp = doReq(t, app, viewerHeader, http.MethodGet, "/api/v1/invoices/inv-1/attachments/att-1/download", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer download status = %d, want 200", resp.StatusCode)
	}
}

// AC (#40): POST /api/v1/invoices/:id/match is invoice:write (finance/
// admin — the finance reviewer persona) — buyer and viewer are denied 403
// at the edge before any procurement call — and the finance call forwards
// the path id with the claims as gRPC metadata, answering 200 with the
// updated invoice.
func TestMatchInvoiceRoute(t *testing.T) {
	for name, role := range map[string]string{
		"buyer":  rbac.RoleBuyer,
		"viewer": rbac.RoleViewer,
	} {
		app, fake, _, sign := invoiceTestApp(t, nil)
		fake.matchRes = sampleInvoice()
		resp, body := reqDo(t, app, "POST", "/api/v1/invoices/inv-1/match", sign(role), "", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["required"] != rbac.InvoiceWrite {
			t.Fatalf("%s: body = %v, want required %s", name, body, rbac.InvoiceWrite)
		}
		if fake.matchReq != nil {
			t.Fatalf("%s: procurement called despite the denial", name)
		}
	}
	app, fake, header, _ := invoiceTestApp(t, nil)
	matched := sampleInvoice()
	matched.Status = "matched"
	matched.MatchResultJson = `{"matched":true,"lines":[]}`
	fake.matchRes = matched
	resp, body := reqDo(t, app, "POST", "/api/v1/invoices/inv-1/match", header.Get("Authorization")[7:], "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finance match status = %d, body = %v", resp.StatusCode, body)
	}
	if body["status"] != "matched" || body["match_result_json"] != `{"matched":true,"lines":[]}` {
		t.Fatalf("match body = %v", body)
	}
	if fake.matchReq.GetId() != "inv-1" {
		t.Fatalf("forwarded id = %q, want inv-1", fake.matchReq.GetId())
	}
	md := metadataOf(t, fake.matchCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" || md["x-role"] != rbac.RoleFinance {
		t.Fatalf("propagated metadata = %v", md)
	}
}

// AC (#41): GET /api/v1/invoices is wired through server.New with
// WithProcurement, invoice:read (the broadest read — viewer passes) grants
// access, the envelope is {invoices, total, page, page_size} with
// header-only rows, the query params ride through to procurement, and the
// verified claims are propagated as gRPC metadata.
func TestInvoiceListRouteThroughServerNew(t *testing.T) {
	// A header-only row, as procurement's list guarantees (no items).
	headerRow := sampleInvoice()
	headerRow.Items = nil
	fake := &fakeInvoiceClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		createRes:             sampleInvoice(),
		getRes:                sampleInvoice(),
		listRes: &procurementv1.ListInvoicesResponse{
			Invoices: []*procurementv1.Invoice{headerRow}, Total: 1, Page: 2, PageSize: 50,
		},
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })
	token := reqTokenFor(t, priv, kid, rbac.RoleViewer) // invoice:read — every role

	resp, body := reqDo(t, app, "GET", "/api/v1/invoices?q=%20INV%2F20%20&status=matched&page=2&page_size=50", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body = %v", resp.StatusCode, body)
	}
	if body["total"] != float64(1) || body["page"] != float64(2) || body["page_size"] != float64(50) {
		t.Fatalf("envelope = %v", body)
	}
	rows, _ := body["invoices"].([]any)
	if len(rows) != 1 {
		t.Fatalf("invoices = %v", body["invoices"])
	}
	row, _ := rows[0].(map[string]any)
	if row["number"] != "INV/2026/0001" || row["status"] != "pending" {
		t.Fatalf("row = %v", row)
	}
	items, _ := row["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("list row carries items: %v", row["items"])
	}
	// The filters were forwarded trimmed; claims ride along as gRPC metadata.
	if fake.listReq.GetQuery() != "INV/20" || fake.listReq.GetStatus() != "matched" ||
		fake.listReq.GetPage() != 2 || fake.listReq.GetPageSize() != 50 {
		t.Fatalf("forwarded list request = %+v", fake.listReq)
	}
	md := metadataOf(t, fake.listCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" {
		t.Fatalf("metadata = %v", md)
	}
}

// AC (#41): the list route requires a Bearer token (401 anonymous before any
// procurement call) and renders the list-filter machine codes — 400
// inv_status_invalid / inv_page_invalid with their human sentences — through
// the shared mapping.
func TestInvoiceListRouteErrors(t *testing.T) {
	app, fake, _, _ := invoiceTestApp(t, nil)
	resp, _ := reqDo(t, app, "GET", "/api/v1/invoices", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}
	if fake.listReq != nil {
		t.Fatalf("procurement called without a token")
	}

	for name, tc := range map[string]struct {
		query     string
		err       error
		want      int
		wantCode  string
		wantError string
	}{
		"bad status": {"status=paused", status.Error(codes.InvalidArgument, "inv_status_invalid"),
			http.StatusBadRequest, "inv_status_invalid", "status must be pending, matched, or discrepancy"},
		"bad page": {"page=2000000", status.Error(codes.InvalidArgument, "inv_page_invalid"),
			http.StatusBadRequest, "inv_page_invalid", "page is out of range"},
	} {
		app, fake, header, _ := invoiceTestApp(t, nil)
		fake.listErr = tc.err
		resp, body := reqDo(t, app, "GET", "/api/v1/invoices?"+tc.query, tokenOf(header), "", nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != tc.wantCode || body["error"] != tc.wantError {
			t.Fatalf("%s: body = %v, want (%q, %q)", name, body, tc.wantCode, tc.wantError)
		}
	}
}

// AC (#40): the match route's error mapping — the re-run guard renders 409
// with the inv_already_matched machine code, unknown invoices 404, bad ids
// 400 — all through the shared mapping.
func TestMatchInvoiceRouteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"already matched": {status.Error(codes.Aborted, "inv_already_matched"), http.StatusConflict, "inv_already_matched"},
		"not found":       {status.Error(codes.NotFound, "inv_not_found"), http.StatusNotFound, "inv_not_found"},
		"bad id":          {status.Error(codes.InvalidArgument, "inv_id_invalid"), http.StatusBadRequest, "inv_id_invalid"},
	} {
		app, fake, header, _ := invoiceTestApp(t, nil)
		fake.matchErr = tc.err
		resp, body := reqDo(t, app, "POST", "/api/v1/invoices/inv-1/match", header.Get("Authorization")[7:], "", nil)
		if resp.StatusCode != tc.code {
			t.Fatalf("%s: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["code"] != tc.want {
			t.Fatalf("%s: code = %v, want %q", name, body["code"], tc.want)
		}
	}
}
