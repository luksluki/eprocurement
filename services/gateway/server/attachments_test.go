package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// fakeDocumentsClient records propagated metadata and replies with canned
// responses or errors.
type fakeDocumentsClient struct {
	lastCtx context.Context

	createReq  *documentsv1.CreateUploadRequest
	createResp *documentsv1.CreateUploadResponse
	createErr  error

	completeResp *documentsv1.Attachment
	completeErr  error

	listReq  *documentsv1.ListAttachmentsRequest
	listResp *documentsv1.ListAttachmentsResponse
	listErr  error

	downloadResp *documentsv1.GetDownloadUrlResponse
	downloadErr  error

	listEventsReq  *documentsv1.ListEventsRequest
	listEventsResp *documentsv1.ListEventsResponse
	listEventsErr  error

	listRunsReq  *documentsv1.ListExportRunsRequest
	listRunsResp *documentsv1.ListExportRunsResponse
	listRunsErr  error

	exportDownloadReq  *documentsv1.GetExportDownloadUrlRequest
	exportDownloadResp *documentsv1.GetExportDownloadUrlResponse
	exportDownloadErr  error

	listAuditReq  *documentsv1.ListAuditRequest
	listAuditResp *documentsv1.ListAuditResponse
	listAuditErr  error

	exportAuditResp *documentsv1.ExportAuditResponse
	exportAuditErr  error
}

func (f *fakeDocumentsClient) CreateUpload(ctx context.Context, in *documentsv1.CreateUploadRequest, opts ...grpc.CallOption) (*documentsv1.CreateUploadResponse, error) {
	f.lastCtx, f.createReq = ctx, in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResp, nil
}

func (f *fakeDocumentsClient) CompleteUpload(ctx context.Context, in *documentsv1.CompleteUploadRequest, opts ...grpc.CallOption) (*documentsv1.Attachment, error) {
	f.lastCtx = ctx
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	return f.completeResp, nil
}

func (f *fakeDocumentsClient) ListAttachments(ctx context.Context, in *documentsv1.ListAttachmentsRequest, opts ...grpc.CallOption) (*documentsv1.ListAttachmentsResponse, error) {
	f.lastCtx, f.listReq = ctx, in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}

func (f *fakeDocumentsClient) GetDownloadUrl(ctx context.Context, in *documentsv1.GetDownloadUrlRequest, opts ...grpc.CallOption) (*documentsv1.GetDownloadUrlResponse, error) {
	f.lastCtx = ctx
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.downloadResp, nil
}

func (f *fakeDocumentsClient) ListEvents(ctx context.Context, in *documentsv1.ListEventsRequest, opts ...grpc.CallOption) (*documentsv1.ListEventsResponse, error) {
	f.lastCtx, f.listEventsReq = ctx, in
	if f.listEventsErr != nil {
		return nil, f.listEventsErr
	}
	return f.listEventsResp, nil
}

func (f *fakeDocumentsClient) ListExportRuns(ctx context.Context, in *documentsv1.ListExportRunsRequest, opts ...grpc.CallOption) (*documentsv1.ListExportRunsResponse, error) {
	f.lastCtx, f.listRunsReq = ctx, in
	if f.listRunsErr != nil {
		return nil, f.listRunsErr
	}
	return f.listRunsResp, nil
}

func (f *fakeDocumentsClient) GetExportDownloadUrl(ctx context.Context, in *documentsv1.GetExportDownloadUrlRequest, opts ...grpc.CallOption) (*documentsv1.GetExportDownloadUrlResponse, error) {
	f.lastCtx, f.exportDownloadReq = ctx, in
	if f.exportDownloadErr != nil {
		return nil, f.exportDownloadErr
	}
	return f.exportDownloadResp, nil
}

func (f *fakeDocumentsClient) ListAudit(ctx context.Context, in *documentsv1.ListAuditRequest, opts ...grpc.CallOption) (*documentsv1.ListAuditResponse, error) {
	f.lastCtx, f.listAuditReq = ctx, in
	if f.listAuditErr != nil {
		return nil, f.listAuditErr
	}
	return f.listAuditResp, nil
}

func (f *fakeDocumentsClient) ExportAudit(ctx context.Context, in *documentsv1.ExportAuditRequest, opts ...grpc.CallOption) (*documentsv1.ExportAuditResponse, error) {
	f.lastCtx = ctx
	if f.exportAuditErr != nil {
		return nil, f.exportAuditErr
	}
	return f.exportAuditResp, nil
}

// attachmentTestApp wires vendor + attachment routes against fakes and
// returns a buyer header plus a signer for tokens with other roles.
func attachmentTestApp(t *testing.T) (*fiber.App, *fakeVendorClient, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	vendors := &fakeVendorClient{vendor: &procurementv1.Vendor{
		Id: "v-1", TenantId: "tenant-9", Code: "G-01", Name: "PT Contoh", Status: "active",
	}}
	documents := &fakeDocumentsClient{}

	app, verifier, priv, kid := newTestApp(t)
	registerVendorRoutes(app, verifier, vendors, documents)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-9", TenantID: "tenant-9", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleBuyer))
	header.Set("Content-Type", "application/json")
	return app, vendors, documents, header, sign
}

func doReq(t *testing.T, app *fiber.App, header http.Header, method, path, body string) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	for key := range header {
		req.Header.Set(key, header.Get(key))
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test %s %s: %v", method, path, err)
	}
	return resp
}

func TestCreateAttachmentReturnsUploadURL(t *testing.T) {
	app, _, documents, header, _ := attachmentTestApp(t)
	documents.createResp = &documentsv1.CreateUploadResponse{
		Attachment: &documentsv1.Attachment{
			Id: "att-1", TenantId: "tenant-9", EntityType: "vendor", EntityId: "v-1",
			Filename: "contract.pdf", MimeType: "application/pdf", SizeBytes: 12,
			ChecksumSha256: strings.Repeat("ab", 32), Status: "pending",
			ObjectKey: "attachments/tenant-9/vendor/att-1", CreatedBy: "user-9",
			CreatedAt: "2026-10-21T10:00:00Z",
		},
		UploadUrl: "http://minio:9000/attachments/tenant-9/vendor/att-1?X-Amz-Signature=sig",
		Method:    "PUT", ExpiresAt: "2026-10-21T10:15:00Z",
	}

	resp := doReq(t, app, header, http.MethodPost, "/api/v1/vendors/v-1/attachments",
		`{"filename":"contract.pdf","mime_type":"application/pdf","size_bytes":12,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Upload struct {
			URL     string `json:"url"`
			Method  string `json:"method"`
			Headers struct {
				ContentType string `json:"Content-Type"`
			} `json:"headers"`
		} `json:"upload"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if !strings.Contains(out.Upload.URL, "X-Amz-Signature") || out.Upload.Method != "PUT" ||
		out.Upload.Headers.ContentType != "application/pdf" || out.Status != "pending" {
		t.Fatalf("body = %s", body)
	}

	// The gateway propagated the tenant to documents.
	md, ok := metadata.FromOutgoingContext(documents.lastCtx)
	if !ok || len(md.Get(rbac.MDTenantID)) != 1 || md.Get(rbac.MDTenantID)[0] != "tenant-9" {
		t.Fatal("tenant metadata not propagated to documents")
	}
	if documents.createReq.GetEntityType() != "vendor" || documents.createReq.GetEntityId() != "v-1" {
		t.Fatalf("create request = %+v", documents.createReq)
	}
}

func TestCreateAttachmentUnknownVendor(t *testing.T) {
	app, vendors, documents, header, _ := attachmentTestApp(t)
	vendors.err = status.Error(codes.NotFound, "vendor_not_found")

	resp := doReq(t, app, header, http.MethodPost, "/api/v1/vendors/nope/attachments",
		`{"filename":"a.pdf","mime_type":"application/pdf","size_bytes":1,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"vendor_not_found"`) {
		t.Fatalf("body = %s", body)
	}
	if documents.lastCtx != nil {
		t.Fatal("documents must not be called for an unknown vendor")
	}
}

func TestCompleteAndDownloadAttachment(t *testing.T) {
	app, _, documents, header, _ := attachmentTestApp(t)
	documents.completeResp = &documentsv1.Attachment{
		Id: "att-1", EntityId: "v-1", Status: "ready", MimeType: "application/pdf",
	}
	resp := doReq(t, app, header, http.MethodPost, "/api/v1/vendors/v-1/attachments/att-1/complete", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status = %d", resp.StatusCode)
	}

	// Attachment of another vendor id: not found, no leak.
	documents.completeResp = &documentsv1.Attachment{Id: "att-2", EntityId: "v-OTHER", Status: "ready"}
	resp = doReq(t, app, header, http.MethodPost, "/api/v1/vendors/v-1/attachments/att-2/complete", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-vendor complete status = %d, want 404", resp.StatusCode)
	}

	documents.downloadResp = &documentsv1.GetDownloadUrlResponse{
		Attachment:  &documentsv1.Attachment{Id: "att-1", EntityId: "v-1", Status: "ready"},
		DownloadUrl: "http://minio:9000/attachments/t/v/att-1?sig",
		ExpiresAt:   "2026-10-21T10:15:00Z",
	}
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/vendors/v-1/attachments/att-1/download", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"download":{`) || !strings.Contains(string(body), `"url":"http://minio`) {
		t.Fatalf("body = %s", body)
	}

	// Pending attachment download maps FailedPrecondition → 412 with code.
	documents.downloadResp = nil
	documents.downloadErr = status.Error(codes.FailedPrecondition, "attachment_not_ready")
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/vendors/v-1/attachments/att-1/download", "")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("pending download status = %d, want 412", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"attachment_not_ready"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestVendorDetailEmbedsAttachments(t *testing.T) {
	app, _, documents, header, _ := attachmentTestApp(t)
	documents.listResp = &documentsv1.ListAttachmentsResponse{Attachments: []*documentsv1.Attachment{
		{Id: "att-1", EntityId: "v-1", Filename: "contract.pdf", MimeType: "application/pdf", Status: "ready"},
		{Id: "att-2", EntityId: "v-1", Filename: "npwp.png", MimeType: "image/png", Status: "pending"},
	}}

	resp := doReq(t, app, header, http.MethodGet, "/api/v1/vendors/v-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vendor detail status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Attachments []map[string]any `json:"attachments"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(out.Attachments) != 2 || out.Attachments[0]["filename"] != "contract.pdf" {
		t.Fatalf("attachments = %s", body)
	}
	if strings.Contains(string(body), "object_key") {
		t.Fatalf("object_key leaked into REST response: %s", body)
	}
}

func TestAttachmentRoutesRequirePermission(t *testing.T) {
	app, _, _, header, sign := attachmentTestApp(t)
	viewerHeader := header.Clone()
	viewerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleViewer))

	for _, target := range []string{
		"POST /api/v1/vendors/v-1/attachments",
		"POST /api/v1/vendors/v-1/attachments/a/complete",
	} {
		parts := strings.SplitN(target, " ", 2)
		resp := doReq(t, app, viewerHeader, parts[0], parts[1], "{}")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as viewer: status = %d, want 403", target, resp.StatusCode)
		}
	}
}

// requisitionAttachmentTestApp wires requisition + attachment routes
// against fakes. The requisition fake replies with a draft owned by user-29
// (the token's subject) unless overridden.
func requisitionAttachmentTestApp(t *testing.T) (*fiber.App, *fakeRequisitionClient, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	requisitions := &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}, getRes: sampleRequisition()}
	documents := &fakeDocumentsClient{}

	app, verifier, priv, kid := newTestApp(t)
	registerRequisitionRoutes(app, verifier, requisitions, documents)

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
	header.Set("Authorization", "Bearer "+sign(rbac.RoleRequester))
	header.Set("Content-Type", "application/json")
	return app, requisitions, documents, header, sign
}

// Declare on a draft the caller owns: the requisition is fetched first (the
// vendor-pattern existence check), documents receives entity_type
// "requisition" plus the requisition's id, and the upload URL envelope comes
// back unchanged.
func TestRequisitionAttachmentDeclare(t *testing.T) {
	app, requisitions, documents, header, _ := requisitionAttachmentTestApp(t)
	documents.createResp = &documentsv1.CreateUploadResponse{
		Attachment: &documentsv1.Attachment{
			Id: "att-1", TenantId: "tenant-29", EntityType: "requisition", EntityId: "pr-1",
			Filename: "quote.pdf", MimeType: "application/pdf", SizeBytes: 12,
			ChecksumSha256: strings.Repeat("ab", 32), Status: "pending",
			CreatedBy: "user-29", CreatedAt: "2026-10-25T10:00:00Z",
		},
		UploadUrl: "http://minio:9000/attachments/tenant-29/requisition/att-1?X-Amz-Signature=sig",
		Method:    "PUT", ExpiresAt: "2026-10-25T10:15:00Z",
	}

	resp := doReq(t, app, header, http.MethodPost, "/api/v1/requisitions/pr-1/attachments",
		`{"filename":"quote.pdf","mime_type":"application/pdf","size_bytes":12,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("declare status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"entity_type":"requisition"`) ||
		!strings.Contains(string(body), "X-Amz-Signature") {
		t.Fatalf("body = %s", body)
	}
	if requisitions.getReq.GetId() != "pr-1" {
		t.Fatalf("existence check = %+v", requisitions.getReq)
	}
	if documents.createReq.GetEntityType() != "requisition" || documents.createReq.GetEntityId() != "pr-1" {
		t.Fatalf("documents request = %+v", documents.createReq)
	}
}

// Declare on another requester's draft of the same tenant: the edge mirrors
// procurement's ownership rule (403 requisition_forbidden) — attachments on
// a requisition are writes to it. An admin is excepted, and an unknown
// requisition 404s before any documents call.
func TestRequisitionAttachmentDeclareOwnership(t *testing.T) {
	app, requisitions, documents, header, sign := requisitionAttachmentTestApp(t)

	// Not the requester, not an admin.
	other := sampleRequisition()
	other.RequesterId = "user-OTHER"
	requisitions.getRes = other
	resp := doReq(t, app, header, http.MethodPost, "/api/v1/requisitions/pr-1/attachments", `{"filename":"a.pdf","mime_type":"application/pdf","size_bytes":1,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign draft status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"requisition_forbidden"`) {
		t.Fatalf("body = %s", body)
	}
	if documents.createReq != nil {
		t.Fatal("documents must not be called for a foreign draft")
	}

	// Admins may act on any requisition of the tenant (requireOwner parity).
	adminHeader := header.Clone()
	adminHeader.Set("Authorization", "Bearer "+sign(rbac.RoleAdmin))
	documents.createResp = &documentsv1.CreateUploadResponse{
		Attachment: &documentsv1.Attachment{Id: "att-9", EntityType: "requisition", EntityId: "pr-1", Status: "pending"},
		UploadUrl:  "http://minio/x", Method: "PUT", ExpiresAt: "z",
	}
	resp = doReq(t, app, adminHeader, http.MethodPost, "/api/v1/requisitions/pr-1/attachments", `{"filename":"a.pdf","mime_type":"application/pdf","size_bytes":1,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin declare status = %d, want 201", resp.StatusCode)
	}

	// Unknown requisition: procurement's 404 rides through, documents untouched.
	requisitions.getErr = status.Error(codes.NotFound, "requisition_not_found")
	documents.createReq = nil
	resp = doReq(t, app, header, http.MethodPost, "/api/v1/requisitions/nope/attachments", `{"filename":"a.pdf","mime_type":"application/pdf","size_bytes":1,"checksum_sha256":"`+strings.Repeat("ab", 32)+`"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown requisition status = %d, want 404", resp.StatusCode)
	}
	if documents.createReq != nil {
		t.Fatal("documents must not be called for an unknown requisition")
	}
}

// Complete and download bind the attachment to the path's requisition id
// (vendor-route parity): another requisition's attachment is a 404, and
// documents' machine codes ride through with the shared mapping.
func TestRequisitionAttachmentCompleteDownload(t *testing.T) {
	app, _, documents, header, _ := requisitionAttachmentTestApp(t)

	documents.completeResp = &documentsv1.Attachment{Id: "att-1", EntityId: "pr-1", Status: "ready"}
	resp := doReq(t, app, header, http.MethodPost, "/api/v1/requisitions/pr-1/attachments/att-1/complete", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status = %d", resp.StatusCode)
	}

	documents.completeResp = &documentsv1.Attachment{Id: "att-2", EntityId: "pr-OTHER", Status: "ready"}
	resp = doReq(t, app, header, http.MethodPost, "/api/v1/requisitions/pr-1/attachments/att-2/complete", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-requisition complete status = %d, want 404", resp.StatusCode)
	}

	documents.downloadResp = &documentsv1.GetDownloadUrlResponse{
		Attachment:  &documentsv1.Attachment{Id: "att-1", EntityId: "pr-1", Status: "ready"},
		DownloadUrl: "http://minio:9000/attachments/t/r/att-1?sig",
		ExpiresAt:   "2026-10-25T10:15:00Z",
	}
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/requisitions/pr-1/attachments/att-1/download", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"download":{`) {
		t.Fatalf("body = %s", body)
	}

	// Checksum mismatch from documents: 412 with the machine code — the UI
	// maps this per-step (#33).
	documents.downloadResp = nil
	documents.downloadErr = status.Error(codes.FailedPrecondition, "attachment_checksum_mismatch")
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/requisitions/pr-1/attachments/att-1/download", "")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("mismatch status = %d, want 412", resp.StatusCode)
	}
}

// The entity-invalid message is neutralized because the map is shared by
// vendor and requisition routes ("owner type", not "vendor").
func TestAttachmentEntityInvalidMessageNeutralized(t *testing.T) {
	if got := attachmentMessages["attachment_entity_invalid"]; got != "attachment owner type is not supported" {
		t.Fatalf("attachment_entity_invalid message = %q", got)
	}
}

// Viewer holds requisition:read but not requisition:write: declare/complete
// are 403 at the edge, download is allowed.
func TestRequisitionAttachmentPermissions(t *testing.T) {
	app, _, _, header, sign := requisitionAttachmentTestApp(t)
	viewerHeader := header.Clone()
	viewerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleViewer))

	for _, target := range []string{
		"POST /api/v1/requisitions/pr-1/attachments",
		"POST /api/v1/requisitions/pr-1/attachments/a/complete",
	} {
		parts := strings.SplitN(target, " ", 2)
		resp := doReq(t, app, viewerHeader, parts[0], parts[1], "{}")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as viewer: status = %d, want 403", target, resp.StatusCode)
		}
	}
}
