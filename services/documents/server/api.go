package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/documents/attachment"
	"github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
)

// presignTTL bounds both upload and download URL validity.
const presignTTL = 15 * time.Minute

// entityPermissions maps an attachment owner kind to the permissions its
// attachments require (#33: vendors and requisitions live under different
// areas of the rbac matrix; #39 adds invoices). The attachment gRPC methods are absent from the
// interceptor's methodPermissions map on purpose — the interceptor cannot
// know the entity type before the call runs — so each handler re-checks the
// per-entity permission here, as soon as the entity type is known (for
// CreateUpload/ListAttachments from the request, for CompleteUpload/
// GetDownloadUrl from the stored attachment). This is the same
// defense-in-depth depth the interceptor provides for single-entity methods.
type entityPerm struct{ read, write string }

var entityPermissions = map[string]entityPerm{
	attachment.EntityTypeVendor:      {rbac.VendorRead, rbac.VendorWrite},
	attachment.EntityTypeRequisition: {rbac.RequisitionRead, rbac.RequisitionWrite},
	attachment.EntityTypeInvoice:     {rbac.InvoiceRead, rbac.InvoiceWrite},
}

// requireEntityWrite enforces the entity's write permission (declare,
// complete) against the caller's roles; requireEntityRead the read side
// (list, download). An unknown entity type is the domain's
// attachment_entity_invalid (InvalidArgument), never a silent pass.
func requireEntityWrite(auth *rbac.AuthContext, entityType string) error {
	perm, known := entityPermissions[entityType]
	if !known {
		return status.Error(codes.InvalidArgument, attachment.ErrEntityInvalid.Error())
	}
	if !rbac.Check(auth.Roles, perm.write) {
		return status.Errorf(codes.PermissionDenied, "permission_denied: requires %s", perm.write)
	}
	return nil
}

func requireEntityRead(auth *rbac.AuthContext, entityType string) error {
	perm, known := entityPermissions[entityType]
	if !known {
		return status.Error(codes.InvalidArgument, attachment.ErrEntityInvalid.Error())
	}
	if !rbac.Check(auth.Roles, perm.read) {
		return status.Errorf(codes.PermissionDenied, "permission_denied: requires %s", perm.read)
	}
	return nil
}

// api adapts the attachment and history domains to the documents.v1 gRPC
// contract. Every method reads the caller identity the gateway propagated
// (tenant scoping, audit ids); attachment methods re-check their per-entity
// permission in-handler (entityPermissions) since the interceptor map cannot
// express it.
type api struct {
	documentsv1.UnimplementedDocumentsServer
	store   *attachment.Store
	storage attachment.Storage
	limits  attachment.Limits
	events  *history.Store
	exports *export.Store
	now     func() time.Time
	log     *slog.Logger
}

// authTenant returns the caller's auth context, rejecting calls that
// reached this far without one.
func authTenant(ctx context.Context) (*rbac.AuthContext, error) {
	auth := rbac.FromMetadata(ctx)
	if auth == nil || auth.TenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "auth context missing: no caller tenant")
	}
	return auth, nil
}

func (a *api) CreateUpload(ctx context.Context, req *documentsv1.CreateUploadRequest) (*documentsv1.CreateUploadResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	// Per-entity permission re-check (the interceptor map cannot express it):
	// the entity type is right here in the request.
	if err := requireEntityWrite(auth, req.GetEntityType()); err != nil {
		return nil, err
	}
	filename := strings.TrimSpace(req.GetFilename())
	mime, checksum := attachment.NormalizeCanonical(req.GetMimeType(), req.GetChecksumSha256())
	if err := attachment.ValidateCreate(a.limits, req.GetEntityType(), req.GetEntityId(),
		filename, mime, req.GetSizeBytes(), checksum); err != nil {
		return nil, mapAttachmentError(err)
	}
	att := attachment.Attachment{
		ID:             uuid.NewString(),
		TenantID:       auth.TenantID,
		EntityType:     req.GetEntityType(),
		EntityID:       req.GetEntityId(),
		Filename:       filename,
		MimeType:       mime,
		SizeBytes:      req.GetSizeBytes(),
		ChecksumSHA256: checksum,
		Status:         attachment.StatusPending,
		ObjectKey:      attachment.ObjectKey(auth.TenantID, req.GetEntityType(), ""),
		CreatedBy:      auth.UserID,
		CreatedAt:      a.now(),
	}
	att.ObjectKey = attachment.ObjectKey(auth.TenantID, att.EntityType, att.ID)
	if err := a.store.Insert(ctx, att); err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	uploadURL, _, err := a.storage.PresignPut(ctx, att.ObjectKey, presignTTL)
	if err != nil {
		_ = a.store.Delete(ctx, att.TenantID, att.ID) // unusable URL: drop the pending metadata
		a.log.Error("presign upload failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &documentsv1.CreateUploadResponse{
		Attachment: attachmentPB(att),
		UploadUrl:  uploadURL,
		Method:     "PUT",
		ExpiresAt:  att.CreatedAt.Add(presignTTL).UTC().Format(time.RFC3339),
	}, nil
}

func (a *api) CompleteUpload(ctx context.Context, req *documentsv1.CompleteUploadRequest) (*documentsv1.Attachment, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	att, err := a.store.Get(ctx, auth.TenantID, req.GetAttachmentId())
	if err != nil {
		return nil, mapAttachmentError(err)
	}
	if err := requireEntityWrite(auth, att.EntityType); err != nil {
		return nil, err
	}
	if att.Status == attachment.StatusReady {
		return attachmentPB(att), nil // idempotent re-complete
	}
	info, err := a.storage.Stat(ctx, att.ObjectKey)
	if err != nil {
		if errors.Is(err, attachment.ErrObjectMissing) {
			return nil, status.Error(codes.FailedPrecondition, attachment.ErrObjectMissing.Error())
		}
		a.log.Error("stat object failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if info.Size != att.SizeBytes {
		a.removeObject(att)
		return nil, status.Error(codes.FailedPrecondition, attachment.ErrSizeMismatch.Error())
	}
	if !attachment.AllowedMimes[info.ContentType] {
		a.removeObject(att)
		return nil, status.Error(codes.FailedPrecondition, attachment.ErrMimeMismatch.Error())
	}
	sum, err := a.hashObject(ctx, att.ObjectKey)
	if err != nil {
		a.log.Error("read object failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if sum != att.ChecksumSHA256 {
		a.removeObject(att)
		return nil, status.Error(codes.FailedPrecondition, attachment.ErrChecksumMismatch.Error())
	}
	if err := a.store.MarkReady(ctx, auth.TenantID, att.ID, info.Size, a.now()); err != nil {
		return nil, mapAttachmentError(err)
	}
	att.Status = attachment.StatusReady
	att.SizeBytes = info.Size
	att.UploadedAt = a.now()
	return attachmentPB(att), nil
}

func (a *api) ListAttachments(ctx context.Context, req *documentsv1.ListAttachmentsRequest) (*documentsv1.ListAttachmentsResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireEntityRead(auth, req.GetEntityType()); err != nil {
		return nil, err
	}
	list, err := a.store.List(ctx, auth.TenantID, req.GetEntityType(), req.GetEntityId())
	if err != nil {
		return nil, mapAttachmentError(err)
	}
	out := &documentsv1.ListAttachmentsResponse{}
	for _, att := range list {
		out.Attachments = append(out.Attachments, attachmentPB(att))
	}
	return out, nil
}

func (a *api) GetDownloadUrl(ctx context.Context, req *documentsv1.GetDownloadUrlRequest) (*documentsv1.GetDownloadUrlResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	att, err := a.store.Get(ctx, auth.TenantID, req.GetAttachmentId())
	if err != nil {
		return nil, mapAttachmentError(err)
	}
	if err := requireEntityRead(auth, att.EntityType); err != nil {
		return nil, err
	}
	if att.Status != attachment.StatusReady {
		return nil, status.Error(codes.FailedPrecondition, attachment.ErrNotReady.Error())
	}
	downloadURL, expiresAt, err := a.storage.PresignGet(ctx, att.ObjectKey, presignTTL)
	if err != nil {
		a.log.Error("presign download failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &documentsv1.GetDownloadUrlResponse{
		Attachment:  attachmentPB(att),
		DownloadUrl: downloadURL,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// ListEvents returns the caller tenant's event history, newest first, with
// keyset pagination. The filter must name a whole aggregate (entity type +
// id, both or neither); the cursor is the opaque continuation token from a
// previous response. payload_json carries the event payload verbatim.
func (a *api) ListEvents(ctx context.Context, req *documentsv1.ListEventsRequest) (*documentsv1.ListEventsResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	events, nextCursor, err := a.events.List(ctx, auth.TenantID, history.ListFilter{
		EntityType: req.GetEntityType(),
		EntityID:   req.GetEntityId(),
		Limit:      int(req.GetLimit()),
		Cursor:     req.GetNextCursor(),
	})
	if err != nil {
		return nil, mapHistoryError(err)
	}
	out := &documentsv1.ListEventsResponse{NextCursor: nextCursor}
	for _, ev := range events {
		out.Events = append(out.Events, eventPB(ev))
	}
	return out, nil
}

// ListExportRuns returns the caller tenant's nightly-export run history,
// newest first (#49, FR-10). The interceptor map already enforced
// export:run; the store scopes every query by the caller's tenant.
func (a *api) ListExportRuns(ctx context.Context, req *documentsv1.ListExportRunsRequest) (*documentsv1.ListExportRunsResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := a.exports.List(ctx, auth.TenantID, int(req.GetLimit()))
	if err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	out := &documentsv1.ListExportRunsResponse{}
	for _, r := range runs {
		out.Runs = append(out.Runs, runPB(r))
	}
	return out, nil
}

// GetExportDownloadUrl returns a presigned GET URL for one successful
// run's CSV (#49). Failed runs have no object (FailedPrecondition
// export_not_succeeded — the basis of #53's error display); a run id of
// another tenant is indistinguishable from a missing one.
func (a *api) GetExportDownloadUrl(ctx context.Context, req *documentsv1.GetExportDownloadUrlRequest) (*documentsv1.GetExportDownloadUrlResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	run, err := a.exports.Get(ctx, auth.TenantID, req.GetRunId())
	if err != nil {
		return nil, mapExportError(err)
	}
	if run.Status != export.StatusSuccess || run.ObjectKey == "" {
		return nil, status.Error(codes.FailedPrecondition, "export_not_succeeded")
	}
	downloadURL, expiresAt, err := a.storage.PresignGet(ctx, run.ObjectKey, presignTTL)
	if err != nil {
		a.log.Error("presign export download failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &documentsv1.GetExportDownloadUrlResponse{
		Run:         runPB(*run),
		DownloadUrl: downloadURL,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// mapExportError keeps the machine code intact: malformed id →
// InvalidArgument, missing/cross-tenant → NotFound.
func mapExportError(err error) error {
	switch {
	case errors.Is(err, export.ErrRunIDInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, export.ErrRunNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// runPB maps a stored run record to the wire message. Timestamps are
// RFC 3339; object_key never crosses the wire (the presigned URL is the
// only download path, the attachment convention).
func runPB(r export.Run) *documentsv1.ExportRun {
	return &documentsv1.ExportRun{
		RunId:          r.RunID,
		TenantId:       r.TenantID,
		ScheduledFor:   r.ScheduledFor.UTC().Format(time.RFC3339),
		Status:         r.Status,
		RowCount:       int32(r.RowCount),
		MissedPrevious: r.MissedPrevious,
		ErrorMessage:   r.ErrorMessage,
		StartedAt:      r.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:     r.FinishedAt.UTC().Format(time.RFC3339),
	}
}

// mapHistoryError keeps the machine code (the gRPC message) intact:
// validation failures → InvalidArgument, anything else → Internal.
func mapHistoryError(err error) error {
	switch {
	case errors.Is(err, history.ErrFilterInvalid),
		errors.Is(err, history.ErrCursorInvalid),
		errors.Is(err, history.ErrEventIDInvalid),
		errors.Is(err, history.ErrEventIncomplete),
		errors.Is(err, history.ErrPayloadInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// eventPB maps a stored history event to the wire message. occurred_at is
// RFC 3339; MongoDB stores milliseconds, and RFC3339Nano prints that
// precision without padding zeros.
func eventPB(ev history.Event) *documentsv1.Event {
	return &documentsv1.Event{
		EventId:     ev.EventID,
		TenantId:    ev.TenantID,
		Type:        ev.Type,
		Version:     ev.Version,
		OccurredAt:  ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		ActorId:     ev.ActorID,
		TraceId:     ev.TraceID,
		EntityType:  ev.EntityType,
		EntityId:    ev.EntityID,
		PayloadJson: ev.PayloadJSON,
	}
}

// removeObject drops bytes that failed verification; the metadata stays
// pending so the caller can upload correct bytes and complete again.
func (a *api) removeObject(att attachment.Attachment) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.storage.Remove(ctx, att.ObjectKey); err != nil {
		a.log.Warn("remove mismatched object failed", "object_key", att.ObjectKey, "error", err)
	}
}

// hashObject streams the stored object once to compute its sha256 hex sum.
func (a *api) hashObject(ctx context.Context, key string) (string, error) {
	reader, err := a.storage.Read(ctx, key)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// mapAttachmentError keeps the machine code (the gRPC message) intact and
// picks the gRPC code: validation failures → InvalidArgument, missing →
// NotFound, lifecycle violations → FailedPrecondition.
func mapAttachmentError(err error) error {
	switch {
	case errors.Is(err, attachment.ErrEntityInvalid),
		errors.Is(err, attachment.ErrFilenameInvalid),
		errors.Is(err, attachment.ErrMimeInvalid),
		errors.Is(err, attachment.ErrTooLarge),
		errors.Is(err, attachment.ErrChecksumInvalid),
		errors.Is(err, attachment.ErrIDInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, attachment.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, attachment.ErrNotReady),
		errors.Is(err, attachment.ErrObjectMissing),
		errors.Is(err, attachment.ErrSizeMismatch),
		errors.Is(err, attachment.ErrMimeMismatch),
		errors.Is(err, attachment.ErrChecksumMismatch):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

func attachmentPB(a attachment.Attachment) *documentsv1.Attachment {
	pb := &documentsv1.Attachment{
		Id: a.ID, TenantId: a.TenantID, EntityType: a.EntityType, EntityId: a.EntityID,
		Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes,
		ChecksumSha256: a.ChecksumSHA256, Status: a.Status, ObjectKey: a.ObjectKey,
		CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !a.UploadedAt.IsZero() {
		pb.UploadedAt = a.UploadedAt.UTC().Format(time.RFC3339)
	}
	return pb
}
