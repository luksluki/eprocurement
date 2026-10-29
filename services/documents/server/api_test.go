package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/documents/attachment"
	"github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
	"github.com/luksluki/eprocurement/services/documents/objectstore"
)

const (
	gTenantA = "eeeeeeee-1111-0000-0000-000000000001"
	gTenantB = "eeeeeeee-1111-0000-0000-000000000002"
	gUser    = "eeeeeeee-1111-0000-0000-0000000000aa"
	gVendor  = "eeeeeeee-1111-0000-0000-0000000000bb"
)

type okPinger struct{}

func (okPinger) Ping(ctx context.Context) error { return nil }

// startDocuments boots the full gRPC wiring (platform server + RBAC map +
// documents API) against a real MongoDB and MinIO.
func startDocuments(t *testing.T) (documentsv1.DocumentsClient, func()) {
	t.Helper()
	if os.Getenv("TEST_MONGO_URI") == "" || os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("TEST_MONGO_URI/TEST_S3_ENDPOINT not set; skipping integration test")
	}
	db := testenv.Mongo(t)
	endpoint, accessKey, secretKey, bucket := testenv.MinIOEnv(t)
	storage, err := objectstore.New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := storage.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	store := attachment.NewStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	events := history.NewStore(db)
	if err := events.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes history: %v", err)
	}
	exports := export.NewStore(db)
	if err := exports.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes export: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	srv := New(log, okPinger{}, store, storage, attachment.Limits{MaxSizeBytes: 1 << 20}, events, exports)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return documentsv1.NewDocumentsClient(conn), func() {
		_ = conn.Close()
		_ = lis.Close()
	}
}

// callerCtx mimics the gateway: user/tenant/roles as outgoing metadata.
func callerCtx(tenantID string, roles ...string) context.Context {
	md := metadata.Pairs(rbac.MDUserID, gUser, rbac.MDTenantID, tenantID)
	for _, r := range roles {
		md.Append(rbac.MDRole, r)
	}
	return metadata.NewOutgoingContext(context.Background(), md)
}

func buyerCtx() context.Context  { return callerCtx(gTenantA, rbac.RoleBuyer) }
func viewerCtx() context.Context { return callerCtx(gTenantA, rbac.RoleViewer) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// declare asks for a presigned upload of content and returns the response.
func declare(t *testing.T, client documentsv1.DocumentsClient, content []byte) *documentsv1.CreateUploadResponse {
	t.Helper()
	resp, err := client.CreateUpload(buyerCtx(), &documentsv1.CreateUploadRequest{
		EntityType:     attachment.EntityTypeVendor,
		EntityId:       gVendor,
		Filename:       "contract.pdf",
		MimeType:       "application/pdf",
		SizeBytes:      int64(len(content)),
		ChecksumSha256: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	return resp
}

func httpPut(t *testing.T, url, contentType string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
}

func TestAttachmentFlowUploadCompleteListDownload(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	content := []byte("vendor contract bytes for the full flow test")
	created := declare(t, client, content)

	att := created.GetAttachment()
	if att.GetStatus() != attachment.StatusPending || att.GetEntityType() != attachment.EntityTypeVendor ||
		att.GetEntityId() != gVendor || att.GetTenantId() != gTenantA {
		t.Fatalf("created attachment = %+v", att)
	}
	if created.GetUploadUrl() == "" || created.GetMethod() != "PUT" || created.GetExpiresAt() == "" {
		t.Fatalf("created response = %+v", created)
	}
	if att.GetObjectKey() != attachment.ObjectKey(gTenantA, attachment.EntityTypeVendor, att.GetId()) {
		t.Fatalf("object key = %q", att.GetObjectKey())
	}

	// Direct-to-MinIO upload via the presigned URL.
	httpPut(t, created.GetUploadUrl(), "application/pdf", content)

	// Download before completion is rejected.
	_, err := client.GetDownloadUrl(buyerCtx(), &documentsv1.GetDownloadUrlRequest{AttachmentId: att.GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != attachment.ErrNotReady.Error() {
		t.Fatalf("pending download error = %v", err)
	}

	ready, err := client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: att.GetId()})
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if ready.GetStatus() != attachment.StatusReady || ready.GetUploadedAt() == "" {
		t.Fatalf("ready attachment = %+v", ready)
	}

	// Complete is idempotent.
	if _, err := client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: att.GetId()}); err != nil {
		t.Fatalf("re-CompleteUpload: %v", err)
	}

	list, err := client.ListAttachments(buyerCtx(), &documentsv1.ListAttachmentsRequest{
		EntityType: attachment.EntityTypeVendor, EntityId: gVendor,
	})
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list.GetAttachments()) != 1 || list.GetAttachments()[0].GetId() != att.GetId() {
		t.Fatalf("list = %+v", list.GetAttachments())
	}

	dl, err := client.GetDownloadUrl(buyerCtx(), &documentsv1.GetDownloadUrlRequest{AttachmentId: att.GetId()})
	if err != nil {
		t.Fatalf("GetDownloadUrl: %v", err)
	}
	if dl.GetDownloadUrl() == "" {
		t.Fatal("empty download url")
	}
	httpResp, err := http.Get(dl.GetDownloadUrl())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer httpResp.Body.Close()
	got, _ := io.ReadAll(httpResp.Body)
	if string(got) != string(content) {
		t.Fatalf("downloaded bytes differ: %q", got)
	}

	// Cross-tenant denial: tenant B cannot see tenant A's attachment.
	_, err = client.GetDownloadUrl(callerCtx(gTenantB, rbac.RoleBuyer),
		&documentsv1.GetDownloadUrlRequest{AttachmentId: att.GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Fatalf("cross-tenant download error = %v", err)
	}
}

func TestCreateUploadValidation(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	tests := []struct {
		name string
		req  *documentsv1.CreateUploadRequest
		want string
	}{
		{"mime", &documentsv1.CreateUploadRequest{
			EntityType: attachment.EntityTypeVendor, EntityId: gVendor,
			Filename: "a.pdf", MimeType: "application/x-msdownload",
			SizeBytes: 10, ChecksumSha256: sha256Hex([]byte("x")),
		}, attachment.ErrMimeInvalid.Error()},
		{"size", &documentsv1.CreateUploadRequest{
			EntityType: attachment.EntityTypeVendor, EntityId: gVendor,
			Filename: "a.pdf", MimeType: "application/pdf",
			SizeBytes: 2 << 20, ChecksumSha256: sha256Hex([]byte("x")), // limit is 1 MiB here
		}, attachment.ErrTooLarge.Error()},
		{"entity", &documentsv1.CreateUploadRequest{
			EntityType: "contract", EntityId: gVendor,
			Filename: "a.pdf", MimeType: "application/pdf",
			SizeBytes: 10, ChecksumSha256: sha256Hex([]byte("x")),
		}, attachment.ErrEntityInvalid.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.CreateUpload(buyerCtx(), tt.req)
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument || st.Message() != tt.want {
				t.Fatalf("error = %v, want InvalidArgument %q", err, tt.want)
			}
		})
	}
}

func TestCompleteUploadVerificationFailures(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	// Declared but never uploaded: the object is missing.
	missing := declare(t, client, []byte("never uploaded"))
	_, err := client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: missing.GetAttachment().GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != attachment.ErrObjectMissing.Error() {
		t.Fatalf("missing-object error = %v", err)
	}

	// Declared checksum does not match the uploaded bytes (same length, so
	// the checksum check is what fires).
	wrong := declare(t, client, []byte("AAAA-AAAA-AAAA"))
	httpPut(t, wrong.GetUploadUrl(), "application/pdf", []byte("BBBB-BBBB-BBBB"))
	_, err = client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: wrong.GetAttachment().GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != attachment.ErrChecksumMismatch.Error() {
		t.Fatalf("checksum error = %v", err)
	}

	// The mismatched object was removed; correct bytes can be re-uploaded
	// into the same pending attachment and complete cleanly.
	httpPut(t, wrong.GetUploadUrl(), "application/pdf", []byte("AAAA-AAAA-AAAA"))
	if _, err := client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: wrong.GetAttachment().GetId()}); err != nil {
		t.Fatalf("CompleteUpload after re-upload: %v", err)
	}

	// Uploaded size differs from the declaration.
	sizeDecl := declare(t, client, []byte("12345678"))
	httpPut(t, sizeDecl.GetUploadUrl(), "application/pdf", []byte("1234"))
	_, err = client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: sizeDecl.GetAttachment().GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != attachment.ErrSizeMismatch.Error() {
		t.Fatalf("size error = %v", err)
	}

	// Uploaded content type is not allowed even though declared mime was.
	mimeDecl := declare(t, client, []byte("executable?"))
	httpPut(t, mimeDecl.GetUploadUrl(), "application/x-msdownload", []byte("executable?"))
	_, err = client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: mimeDecl.GetAttachment().GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != attachment.ErrMimeMismatch.Error() {
		t.Fatalf("mime error = %v", err)
	}
}

func TestAttachmentRBACAndAuth(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	// Viewer lacks vendor:write — now re-checked in-handler per entity type
	// (the attachment methods left the interceptor map in #33).
	_, err := client.CreateUpload(viewerCtx(), &documentsv1.CreateUploadRequest{
		EntityType: attachment.EntityTypeVendor, EntityId: gVendor,
		Filename: "a.pdf", MimeType: "application/pdf",
		SizeBytes: 4, ChecksumSha256: sha256Hex([]byte("x")),
	})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied {
		t.Fatalf("viewer CreateUpload error = %v", err)
	}

	// No auth metadata at all.
	_, err = client.CreateUpload(context.Background(), &documentsv1.CreateUploadRequest{
		EntityType: attachment.EntityTypeVendor, EntityId: gVendor,
		Filename: "a.pdf", MimeType: "application/pdf",
		SizeBytes: 4, ChecksumSha256: sha256Hex([]byte("x")),
	})
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("anonymous CreateUpload error = %v", err)
	}
}

// Requisition attachments (#33): the per-entity permission table — a
// requester (requisition:write) declares, uploads, completes, lists, and
// downloads an attachment owned by a requisition; the same requester is
// denied on vendor-owned attachments (vendor:write), a buyer is denied the
// requisition write, and reads stay broad (viewer lists).
func TestAttachmentRequisitionEntity(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	gRequisition := "eeeeeeee-1111-0000-0000-0000000000cc"
	requesterCtx := callerCtx(gTenantA, rbac.RoleRequester)

	// A requester declares a requisition attachment: pending metadata, object
	// key under attachments/{tenant}/requisition/{uuid}.
	content := []byte("requisition quote bytes")
	created, err := client.CreateUpload(requesterCtx, &documentsv1.CreateUploadRequest{
		EntityType:     attachment.EntityTypeRequisition,
		EntityId:       gRequisition,
		Filename:       "quote.pdf",
		MimeType:       "application/pdf",
		SizeBytes:      int64(len(content)),
		ChecksumSha256: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("CreateUpload requisition: %v", err)
	}
	att := created.GetAttachment()
	if att.GetEntityType() != attachment.EntityTypeRequisition || att.GetEntityId() != gRequisition {
		t.Fatalf("attachment = %+v", att)
	}
	if att.GetObjectKey() != attachment.ObjectKey(gTenantA, attachment.EntityTypeRequisition, att.GetId()) {
		t.Fatalf("object key = %q", att.GetObjectKey())
	}

	// The same requester cannot declare a vendor attachment (vendor:write).
	_, err = client.CreateUpload(requesterCtx, &documentsv1.CreateUploadRequest{
		EntityType: attachment.EntityTypeVendor,
		EntityId:   gVendor,
		Filename:   "a.pdf", MimeType: "application/pdf",
		SizeBytes: 4, ChecksumSha256: sha256Hex([]byte("x")),
	})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "permission_denied: requires vendor:write" {
		t.Fatalf("requester vendor declare error = %v, want PermissionDenied requires vendor:write", err)
	}

	// A buyer cannot complete a requisition attachment (requisition:write),
	// even one that exists — the per-entity write guard.
	httpPut(t, created.GetUploadUrl(), "application/pdf", content)
	_, err = client.CompleteUpload(buyerCtx(), &documentsv1.CompleteUploadRequest{AttachmentId: att.GetId()})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "permission_denied: requires requisition:write" {
		t.Fatalf("buyer complete requisition attachment error = %v", err)
	}

	// The owner completes; list and download follow (requisition:read is
	// broad — a viewer may list and download).
	ready, err := client.CompleteUpload(requesterCtx, &documentsv1.CompleteUploadRequest{AttachmentId: att.GetId()})
	if err != nil || ready.GetStatus() != attachment.StatusReady {
		t.Fatalf("complete = (%+v, %v)", ready, err)
	}
	list, err := client.ListAttachments(viewerCtx(), &documentsv1.ListAttachmentsRequest{
		EntityType: attachment.EntityTypeRequisition, EntityId: gRequisition,
	})
	if err != nil || len(list.GetAttachments()) != 1 || list.GetAttachments()[0].GetId() != att.GetId() {
		t.Fatalf("list = (%+v, %v)", list, err)
	}
	dl, err := client.GetDownloadUrl(viewerCtx(), &documentsv1.GetDownloadUrlRequest{AttachmentId: att.GetId()})
	if err != nil || dl.GetDownloadUrl() == "" {
		t.Fatalf("download = (%+v, %v)", dl, err)
	}

	// Cross-tenant denial on the new entity: tenant B's list of tenant A's
	// requisition owner is empty (tenant-scoped query, no leak, no error).
	listB, err := client.ListAttachments(callerCtx(gTenantB, rbac.RoleViewer),
		&documentsv1.ListAttachmentsRequest{
			EntityType: attachment.EntityTypeRequisition, EntityId: gRequisition,
		})
	if err != nil || len(listB.GetAttachments()) != 0 {
		t.Fatalf("tenant B list = (%+v, %v)", listB, err)
	}

	// Unknown entity on the read path is the domain's machine code, not a
	// silent pass.
	_, err = client.ListAttachments(viewerCtx(), &documentsv1.ListAttachmentsRequest{
		EntityType: "contract", EntityId: gRequisition,
	})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != attachment.ErrEntityInvalid.Error() {
		t.Fatalf("unknown entity list error = %v", err)
	}
}

// Invoice attachments (#39): the per-entity permission table gains the
// invoice area — finance (invoice:write) declares and completes, a buyer is
// denied the write, and reads stay broad (invoice:read; a viewer lists and
// downloads). 'invoice' is now an accepted entity type, so the owner key
// layout applies unchanged.
func TestAttachmentInvoiceEntity(t *testing.T) {
	client, stop := startDocuments(t)
	defer stop()

	gInvoice := "eeeeeeee-2222-0000-0000-0000000000dd"
	financeCtx := callerCtx(gTenantA, rbac.RoleFinance)

	// Finance declares an invoice attachment: pending metadata, object key
	// under attachments/{tenant}/invoice/{uuid}.
	content := []byte("invoice scan bytes")
	created, err := client.CreateUpload(financeCtx, &documentsv1.CreateUploadRequest{
		EntityType:     attachment.EntityTypeInvoice,
		EntityId:       gInvoice,
		Filename:       "invoice-2026-001.pdf",
		MimeType:       "application/pdf",
		SizeBytes:      int64(len(content)),
		ChecksumSha256: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("CreateUpload invoice: %v", err)
	}
	att := created.GetAttachment()
	if att.GetEntityType() != attachment.EntityTypeInvoice || att.GetEntityId() != gInvoice {
		t.Fatalf("attachment = %+v", att)
	}
	if att.GetObjectKey() != attachment.ObjectKey(gTenantA, attachment.EntityTypeInvoice, att.GetId()) {
		t.Fatalf("object key = %q", att.GetObjectKey())
	}

	// A buyer cannot declare an invoice attachment (invoice:write is
	// finance/admin only).
	_, err = client.CreateUpload(buyerCtx(), &documentsv1.CreateUploadRequest{
		EntityType: attachment.EntityTypeInvoice,
		EntityId:   gInvoice,
		Filename:   "a.pdf", MimeType: "application/pdf",
		SizeBytes: 4, ChecksumSha256: sha256Hex([]byte("x")),
	})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "permission_denied: requires invoice:write" {
		t.Fatalf("buyer invoice declare error = %v, want PermissionDenied requires invoice:write", err)
	}

	// Finance completes; a viewer lists and downloads (invoice:read is broad).
	httpPut(t, created.GetUploadUrl(), "application/pdf", content)
	ready, err := client.CompleteUpload(financeCtx, &documentsv1.CompleteUploadRequest{AttachmentId: att.GetId()})
	if err != nil || ready.GetStatus() != attachment.StatusReady {
		t.Fatalf("complete = (%+v, %v)", ready, err)
	}
	list, err := client.ListAttachments(viewerCtx(), &documentsv1.ListAttachmentsRequest{
		EntityType: attachment.EntityTypeInvoice, EntityId: gInvoice,
	})
	if err != nil || len(list.GetAttachments()) != 1 || list.GetAttachments()[0].GetId() != att.GetId() {
		t.Fatalf("list = (%+v, %v)", list, err)
	}
	dl, err := client.GetDownloadUrl(viewerCtx(), &documentsv1.GetDownloadUrlRequest{AttachmentId: att.GetId()})
	if err != nil || dl.GetDownloadUrl() == "" {
		t.Fatalf("download = (%+v, %v)", dl, err)
	}

	// Cross-tenant denial on the invoice entity: tenant B's list of tenant
	// A's invoice owner is empty (tenant-scoped query, no leak, no error).
	listB, err := client.ListAttachments(callerCtx(gTenantB, rbac.RoleViewer),
		&documentsv1.ListAttachmentsRequest{
			EntityType: attachment.EntityTypeInvoice, EntityId: gInvoice,
		})
	if err != nil || len(listB.GetAttachments()) != 0 {
		t.Fatalf("tenant B invoice list = (%+v, %v)", listB, err)
	}
}

// startDocumentsEvents boots the full gRPC wiring for ListEvents tests —
// real MongoDB, no MinIO (ListEvents never touches object storage).
func startDocumentsEvents(t *testing.T) (documentsv1.DocumentsClient, *history.Store, func()) {
	t.Helper()
	if os.Getenv("TEST_MONGO_URI") == "" {
		t.Skip("TEST_MONGO_URI not set; skipping integration test")
	}
	db := testenv.Mongo(t)
	events := history.NewStore(db)
	if err := events.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes history: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	srv := New(log, okPinger{}, attachment.NewStore(db), nil, attachment.Limits{}, events, export.NewStore(db))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return documentsv1.NewDocumentsClient(conn), events, func() {
		_ = conn.Close()
		_ = lis.Close()
	}
}

func seedHistory(t *testing.T, store *history.Store) {
	t.Helper()
	base := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	specs := []struct {
		id, tenant, eventType string
		at                    time.Time
		entity                string
	}{
		// e5/e4 share occurred_at — the approval engine's step + terminal
		// pair — so the event_id DESC tiebreaker decides their order.
		{"e5", gTenantA, "pr.step_approved", base.Add(3 * time.Second), "r-1"},
		{"e4", gTenantA, "pr.approved", base.Add(3 * time.Second), "r-1"},
		{"e3", gTenantA, "pr.submitted", base.Add(2 * time.Second), "r-1"},
		{"e2", gTenantA, "pr.created", base.Add(1 * time.Second), "r-2"},
		{"e1", gTenantA, "pr.created", base, "r-2"},
		{"b1", gTenantB, "pr.created", base, "r-1"},
	}
	for _, sp := range specs {
		ev, err := history.NewEvent(sp.id, sp.tenant, sp.eventType, sp.at,
			gUser, "trace-42", "requisition", sp.entity, `{"number":"PR-000001"}`)
		if err != nil {
			t.Fatalf("NewEvent %s: %v", sp.id, err)
		}
		if _, err := store.Apply(t.Context(), ev); err != nil {
			t.Fatalf("Apply %s: %v", sp.id, err)
		}
	}
}

func eventIDsPB(events []*documentsv1.Event) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.GetEventId())
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListEventsOrderPaginationTenant pins the ListEvents contract over the
// real gRPC wiring: newest first with the event_id tiebreaker, cursor pages
// that neither duplicate nor skip, the entity filter, cross-tenant denial,
// and payload_json fidelity.
func TestListEventsOrderPaginationTenant(t *testing.T) {
	client, store, stop := startDocumentsEvents(t)
	defer stop()
	seedHistory(t, store)

	// Walk the tenant A history page by page (viewer holds requisition:read).
	var walked []string
	cursor := ""
	pages := 0
	for {
		resp, err := client.ListEvents(viewerCtx(), &documentsv1.ListEventsRequest{Limit: 2, NextCursor: cursor})
		if err != nil {
			t.Fatalf("ListEvents page %d: %v", pages+1, err)
		}
		walked = append(walked, eventIDsPB(resp.GetEvents())...)
		pages++
		if resp.GetNextCursor() == "" {
			break
		}
		cursor = resp.GetNextCursor()
	}
	if !equalStrings(walked, []string{"e5", "e4", "e3", "e2", "e1"}) {
		t.Fatalf("walked = %v, want e5..e1 in order", walked)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}

	// Field fidelity of the newest event (the shared-timestamp tie pair).
	resp, err := client.ListEvents(viewerCtx(), &documentsv1.ListEventsRequest{Limit: 1})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	top := resp.GetEvents()[0]
	if top.GetType() != "pr.step_approved" || top.GetVersion() != 1 ||
		top.GetActorId() != gUser || top.GetTraceId() != "trace-42" ||
		top.GetEntityType() != "requisition" || top.GetEntityId() != "r-1" ||
		top.GetPayloadJson() != `{"number":"PR-000001"}` ||
		top.GetOccurredAt() == "" {
		t.Fatalf("newest event fidelity: %+v", top)
	}

	// Entity filter narrows to one aggregate.
	resp, err = client.ListEvents(viewerCtx(), &documentsv1.ListEventsRequest{
		EntityType: "requisition", EntityId: "r-2", Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents filtered: %v", err)
	}
	if !equalStrings(eventIDsPB(resp.GetEvents()), []string{"e2", "e1"}) {
		t.Fatalf("filtered = %v", eventIDsPB(resp.GetEvents()))
	}

	// Cross-tenant denial: tenant B sees none of tenant A's events.
	resp, err = client.ListEvents(callerCtx(gTenantB, rbac.RoleBuyer),
		&documentsv1.ListEventsRequest{Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents tenant B: %v", err)
	}
	if ids := eventIDsPB(resp.GetEvents()); !equalStrings(ids, []string{"b1"}) {
		t.Fatalf("tenant B sees %v, want only b1", ids)
	}
}

// TestListEventsValidation pins the InvalidArgument machine codes: the
// half filter and the malformed cursor. Limit normalizes instead (the
// vendor-list convention) — a limit over the max is clamped, not rejected.
func TestListEventsValidation(t *testing.T) {
	client, store, stop := startDocumentsEvents(t)
	defer stop()
	seedHistory(t, store)

	tests := []struct {
		name string
		req  *documentsv1.ListEventsRequest
		want string
	}{
		{"entity type without id", &documentsv1.ListEventsRequest{EntityType: "requisition"}, history.ErrFilterInvalid.Error()},
		{"entity id without type", &documentsv1.ListEventsRequest{EntityId: "r-1"}, history.ErrFilterInvalid.Error()},
		{"malformed cursor", &documentsv1.ListEventsRequest{NextCursor: "not-a-cursor"}, history.ErrCursorInvalid.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.ListEvents(viewerCtx(), tt.req)
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument || st.Message() != tt.want {
				t.Fatalf("error = %v, want InvalidArgument %q", err, tt.want)
			}
		})
	}

	// Over-max limit clamps (max 100) and still answers.
	resp, err := client.ListEvents(viewerCtx(), &documentsv1.ListEventsRequest{Limit: 1000})
	if err != nil || len(resp.GetEvents()) != 5 {
		t.Fatalf("clamped limit: (%d events, %v)", len(resp.GetEvents()), err)
	}
}

// TestListEventsRBAC pins the permission mapping: ListEvents requires
// requisition:read. Every tenant role holds it, so the denial case uses a
// role outside the matrix (rbac.Check grants unknown roles nothing); a
// call without caller metadata is Unauthenticated. Methods missing from
// the map would pass through unprotected (rbac/grpc.go), so the mapping
// itself is the protection.
func TestListEventsRBAC(t *testing.T) {
	client, store, stop := startDocumentsEvents(t)
	defer stop()
	seedHistory(t, store)

	// Role outside the matrix → no requisition:read → PermissionDenied.
	_, err := client.ListEvents(callerCtx(gTenantA, "courier"),
		&documentsv1.ListEventsRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied {
		t.Fatalf("unknown-role error = %v, want PermissionDenied", err)
	}

	// No caller metadata at all → Unauthenticated.
	_, err = client.ListEvents(context.Background(), &documentsv1.ListEventsRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("anonymous error = %v, want Unauthenticated", err)
	}

	// A least-privileged matrix role (viewer) reads fine.
	if _, err := client.ListEvents(viewerCtx(), &documentsv1.ListEventsRequest{Limit: 1}); err != nil {
		t.Fatalf("viewer ListEvents: %v", err)
	}
}

// startDocumentsExports boots the full wiring for the export run API (#49)
// — real MongoDB + MinIO — and hands the test the export store for seeding
// and the storage adapter for writing the CSV object a successful run
// points at.
func startDocumentsExports(t *testing.T) (documentsv1.DocumentsClient, *export.Store, *objectstore.MinIO, func()) {
	t.Helper()
	db := testenv.Mongo(t)
	endpoint, accessKey, secretKey, bucket := testenv.MinIOEnv(t)
	storage, err := objectstore.New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := storage.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	exports := export.NewStore(db)
	if err := exports.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes export: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	srv := New(log, okPinger{}, attachment.NewStore(db), storage, attachment.Limits{}, history.NewStore(db), exports)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return documentsv1.NewDocumentsClient(conn), exports, storage, func() {
		_ = conn.Close()
		_ = lis.Close()
	}
}

func exportRun(tenantID string, slot time.Time, status string) export.Run {
	run := export.Run{
		RunID:        uuid.NewString(),
		TenantID:     tenantID,
		ScheduledFor: slot,
		Status:       status,
		RowCount:     2,
		StartedAt:    slot.Add(time.Minute),
		FinishedAt:   slot.Add(2 * time.Minute),
	}
	if status == export.StatusSuccess {
		run.ObjectKey = "exports/" + tenantID + "/" + slot.Format("2006-01-02") + "/purchase-orders.csv"
	} else {
		run.ErrorMessage = "object store unreachable"
	}
	return run
}

// TestExportRunsListAndDownload pins the export run API (#49): finance
// (export:run) lists the tenant's runs newest-first with the missed flag
// surfacing, downloads a successful run's CSV through the presigned URL,
// cannot download a failed run, and never sees another tenant's run ids.
func TestExportRunsListAndDownload(t *testing.T) {
	client, store, storage, stop := startDocumentsExports(t)
	defer stop()
	financeCtx := callerCtx(gTenantA, rbac.RoleFinance)

	slot1 := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	slot2 := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	failed := exportRun(gTenantA, slot1, export.StatusFailed)
	success := exportRun(gTenantA, slot2, export.StatusSuccess)
	success.MissedPrevious = true // detection flagged the slot1 failure
	if err := store.UpsertRun(t.Context(), failed); err != nil {
		t.Fatalf("seed failed run: %v", err)
	}
	if err := store.UpsertRun(t.Context(), success); err != nil {
		t.Fatalf("seed success run: %v", err)
	}
	// Tenant B has a successful run of its own.
	otherSuccess := exportRun(gTenantB, slot2, export.StatusSuccess)
	if err := store.UpsertRun(t.Context(), otherSuccess); err != nil {
		t.Fatalf("seed tenant B run: %v", err)
	}
	// Write the CSV object the successful run points at.
	csv := "po_number,vendor_code\nPO-1,V-1\n"
	if err := storage.Put(t.Context(), success.ObjectKey, strings.NewReader(csv), int64(len(csv)), "text/csv; charset=utf-8"); err != nil {
		t.Fatalf("put csv: %v", err)
	}

	// List: newest first, both fields surfaced, tenant-scoped.
	list, err := client.ListExportRuns(financeCtx, &documentsv1.ListExportRunsRequest{})
	if err != nil {
		t.Fatalf("ListExportRuns: %v", err)
	}
	if len(list.GetRuns()) != 2 {
		t.Fatalf("runs = %d, want 2", len(list.GetRuns()))
	}
	newest := list.GetRuns()[0]
	if newest.GetRunId() != success.RunID || newest.GetStatus() != export.StatusSuccess ||
		!newest.GetMissedPrevious() || newest.GetRowCount() != 2 || newest.GetScheduledFor() == "" {
		t.Fatalf("newest run = %+v", newest)
	}
	oldest := list.GetRuns()[1]
	if oldest.GetRunId() != failed.RunID || oldest.GetErrorMessage() == "" {
		t.Fatalf("oldest run = %+v, want the failed run with its error", oldest)
	}

	// Download a successful run: presigned URL serves the exact CSV bytes.
	dl, err := client.GetExportDownloadUrl(financeCtx, &documentsv1.GetExportDownloadUrlRequest{RunId: success.RunID})
	if err != nil {
		t.Fatalf("GetExportDownloadUrl: %v", err)
	}
	if dl.GetDownloadUrl() == "" || dl.GetExpiresAt() == "" || dl.GetRun().GetRunId() != success.RunID {
		t.Fatalf("download response = %+v", dl)
	}
	httpResp, err := http.Get(dl.GetDownloadUrl())
	if err != nil {
		t.Fatalf("GET csv: %v", err)
	}
	defer httpResp.Body.Close()
	got, _ := io.ReadAll(httpResp.Body)
	if string(got) != csv {
		t.Fatalf("csv bytes = %q, want %q", got, csv)
	}

	// A failed run has no object: FailedPrecondition with the machine code.
	_, err = client.GetExportDownloadUrl(financeCtx, &documentsv1.GetExportDownloadUrlRequest{RunId: failed.RunID})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != "export_not_succeeded" {
		t.Fatalf("failed-run download error = %v", err)
	}

	// Cross-tenant denial: tenant B's finance cannot resolve tenant A's run
	// id — indistinguishable from missing.
	_, err = client.GetExportDownloadUrl(callerCtx(gTenantB, rbac.RoleFinance),
		&documentsv1.GetExportDownloadUrlRequest{RunId: success.RunID})
	if st, _ := status.FromError(err); st.Code() != codes.NotFound || st.Message() != export.ErrRunNotFound.Error() {
		t.Fatalf("cross-tenant download error = %v", err)
	}

	// Malformed id: the typed invalid code.
	_, err = client.GetExportDownloadUrl(financeCtx, &documentsv1.GetExportDownloadUrlRequest{RunId: "not-a-uuid"})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != export.ErrRunIDInvalid.Error() {
		t.Fatalf("malformed id error = %v", err)
	}
}

// TestExportRunsRBAC pins the interceptor mapping: export:run is held by
// finance/admin only — a buyer is denied; a caller without metadata is
// Unauthenticated. Methods missing from the map pass through unprotected,
// so the mapping itself is the protection (the ListEvents precedent).
func TestExportRunsRBAC(t *testing.T) {
	client, _, _, stop := startDocumentsExports(t)
	defer stop()

	_, err := client.ListExportRuns(buyerCtx(), &documentsv1.ListExportRunsRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "permission_denied: requires export:run" {
		t.Fatalf("buyer ListExportRuns error = %v, want PermissionDenied requires export:run", err)
	}
	_, err = client.GetExportDownloadUrl(buyerCtx(), &documentsv1.GetExportDownloadUrlRequest{RunId: uuid.NewString()})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied {
		t.Fatalf("buyer GetExportDownloadUrl error = %v, want PermissionDenied", err)
	}
	_, err = client.ListExportRuns(context.Background(), &documentsv1.ListExportRunsRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("anonymous error = %v, want Unauthenticated", err)
	}
}
