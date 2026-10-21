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
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/documents/attachment"
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

	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	srv := New(log, okPinger{}, store, storage, attachment.Limits{MaxSizeBytes: 1 << 20})

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
			EntityType: "requisition", EntityId: gVendor,
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

	// Viewer lacks vendor:write.
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
