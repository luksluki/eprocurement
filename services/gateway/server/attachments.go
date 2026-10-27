package server

import (
	"context"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// DocumentsClient is the documents surface the gateway proxies. The
// generated gRPC client satisfies it; tests substitute a fake.
type DocumentsClient interface {
	CreateUpload(ctx context.Context, in *documentsv1.CreateUploadRequest, opts ...grpc.CallOption) (*documentsv1.CreateUploadResponse, error)
	CompleteUpload(ctx context.Context, in *documentsv1.CompleteUploadRequest, opts ...grpc.CallOption) (*documentsv1.Attachment, error)
	ListAttachments(ctx context.Context, in *documentsv1.ListAttachmentsRequest, opts ...grpc.CallOption) (*documentsv1.ListAttachmentsResponse, error)
	GetDownloadUrl(ctx context.Context, in *documentsv1.GetDownloadUrlRequest, opts ...grpc.CallOption) (*documentsv1.GetDownloadUrlResponse, error)
	ListEvents(ctx context.Context, in *documentsv1.ListEventsRequest, opts ...grpc.CallOption) (*documentsv1.ListEventsResponse, error)
}

// Attachment owner kinds the gateway routes for (documents owns the full
// set; the gateway only needs the ones it has routes for).
const (
	entityVendor      = "vendor"
	entityRequisition = "requisition"
	entityInvoice     = "invoice"
)

// registerAttachmentRoutes wires /api/v1/vendors/:id/attachments under the
// vendors group. The vendor's existence is checked against procurement
// before any documents call, so attachments cannot dangle on unknown ids.
func registerAttachmentRoutes(vendors fiber.Router, verifier *token.Verifier, vendorClient VendorClient, documents DocumentsClient) {
	vendors.Post("/:id/attachments", RequirePermission(rbac.VendorWrite), createAttachment(vendorClient, documents))
	vendors.Post("/:id/attachments/:attachmentId/complete", RequirePermission(rbac.VendorWrite), completeAttachment(documents))
	vendors.Get("/:id/attachments/:attachmentId/download", RequirePermission(rbac.VendorRead), downloadAttachment(documents))
}

// registerRequisitionAttachmentRoutes wires the requisition attachment
// routes (#33): declare/complete/download under
// /api/v1/requisitions/:id/attachments. Declare checks the draft exists in
// procurement first (the vendor pattern) plus the requester-ownership rule
// procurement enforces on edit/submit — attachments on a requisition are
// writes to that requisition, so declaring against another user's draft is
// 403 requisition_forbidden, not a silent pass. Complete/download only bind
// the attachment to the path's requisition id (vendor parity).
func registerRequisitionAttachmentRoutes(reqs fiber.Router, verifier *token.Verifier, requisitions ProcurementClient, documents DocumentsClient) {
	reqs.Post("/:id/attachments", RequirePermission(rbac.RequisitionWrite), createRequisitionAttachment(requisitions, documents))
	reqs.Post("/:id/attachments/:attachmentId/complete", RequirePermission(rbac.RequisitionWrite), completeAttachment(documents))
	reqs.Get("/:id/attachments/:attachmentId/download", RequirePermission(rbac.RequisitionRead), downloadAttachment(documents))
}

// registerInvoiceAttachmentRoutes wires the invoice attachment routes
// (#39): declare/complete/download under /api/v1/invoices/:id/attachments.
// Declare checks the invoice exists in procurement first (the vendor
// pattern); complete/download only bind the attachment to the path's
// invoice id. Permissions are the invoice area's (finance/admin write,
// everyone read) — no ownership rule, invoices belong to the tenant.
func registerInvoiceAttachmentRoutes(invoices fiber.Router, verifier *token.Verifier, procurement ProcurementClient, documents DocumentsClient) {
	invoices.Post("/:id/attachments", RequirePermission(rbac.InvoiceWrite), createInvoiceAttachment(procurement, documents))
	invoices.Post("/:id/attachments/:attachmentId/complete", RequirePermission(rbac.InvoiceWrite), completeAttachment(documents))
	invoices.Get("/:id/attachments/:attachmentId/download", RequirePermission(rbac.InvoiceRead), downloadAttachment(documents))
}

type attachmentBody struct {
	Filename       string `json:"filename"`
	MimeType       string `json:"mime_type"`
	SizeBytes      int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256"`
}

func createAttachment(vendorClient VendorClient, documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body attachmentBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		// The attachment must belong to an existing vendor of this tenant.
		if _, err := vendorClient.GetVendor(forwardContext(c, claims),
			&procurementv1.GetVendorRequest{Id: c.Params("id")}); err != nil {
			return vendorError(c, err)
		}
		resp, err := documents.CreateUpload(forwardContext(c, claims), &documentsv1.CreateUploadRequest{
			EntityType:     entityVendor,
			EntityId:       c.Params("id"),
			Filename:       strings.TrimSpace(body.Filename),
			MimeType:       body.MimeType,
			SizeBytes:      body.SizeBytes,
			ChecksumSha256: body.ChecksumSHA256,
		})
		if err != nil {
			return attachmentError(c, err)
		}
		out := attachmentJSON(resp.GetAttachment())
		out["upload"] = fiber.Map{
			"url":        resp.GetUploadUrl(),
			"method":     resp.GetMethod(),
			"expires_at": resp.GetExpiresAt(),
			"headers":    fiber.Map{"Content-Type": resp.GetAttachment().GetMimeType()},
		}
		return c.Status(fiber.StatusCreated).JSON(out)
	}
}

// createRequisitionAttachment serves POST /api/v1/requisitions/:id/
// attachments (#33). Beyond the vendor pattern's existence check, the
// caller must own the draft: requisition attachments are writes to the
// requisition, and procurement's ownership rule (requester or admin,
// docs/rbac.md) is mirrored here because documents has no knowledge of
// requisition ownership.
func createRequisitionAttachment(requisitions ProcurementClient, documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body attachmentBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		r, err := requisitions.GetRequisition(forwardContext(c, claims),
			&procurementv1.GetRequisitionRequest{Id: c.Params("id")})
		if err != nil {
			return requisitionError(c, err)
		}
		if r.GetRequesterId() != claims.Sub && !slices.Contains(claims.Roles, rbac.RoleAdmin) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error": requisitionMessages["requisition_forbidden"],
				"code":  "requisition_forbidden",
			})
		}
		resp, err := documents.CreateUpload(forwardContext(c, claims), &documentsv1.CreateUploadRequest{
			EntityType:     entityRequisition,
			EntityId:       r.GetId(),
			Filename:       strings.TrimSpace(body.Filename),
			MimeType:       body.MimeType,
			SizeBytes:      body.SizeBytes,
			ChecksumSha256: body.ChecksumSHA256,
		})
		if err != nil {
			return attachmentError(c, err)
		}
		out := attachmentJSON(resp.GetAttachment())
		out["upload"] = fiber.Map{
			"url":        resp.GetUploadUrl(),
			"method":     resp.GetMethod(),
			"expires_at": resp.GetExpiresAt(),
			"headers":    fiber.Map{"Content-Type": resp.GetAttachment().GetMimeType()},
		}
		return c.Status(fiber.StatusCreated).JSON(out)
	}
}

// createInvoiceAttachment serves POST /api/v1/invoices/:id/attachments
// (#39): the vendor pattern — the invoice must exist in procurement (a
// cross-tenant or unknown id answers its 404/400 before any documents
// call), then the declaration goes to documents. The create API never
// requires an attachment (the demo flow does, #41/#40 — maintainer decision
// 2026-10-27); declaring one is this separate step because the architecture
// has no cross-service atomicity.
func createInvoiceAttachment(procurement ProcurementClient, documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body attachmentBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		if _, err := procurement.GetInvoice(forwardContext(c, claims),
			&procurementv1.GetInvoiceRequest{Id: c.Params("id")}); err != nil {
			return invoiceError(c, err)
		}
		resp, err := documents.CreateUpload(forwardContext(c, claims), &documentsv1.CreateUploadRequest{
			EntityType:     entityInvoice,
			EntityId:       c.Params("id"),
			Filename:       strings.TrimSpace(body.Filename),
			MimeType:       body.MimeType,
			SizeBytes:      body.SizeBytes,
			ChecksumSha256: body.ChecksumSHA256,
		})
		if err != nil {
			return attachmentError(c, err)
		}
		out := attachmentJSON(resp.GetAttachment())
		out["upload"] = fiber.Map{
			"url":        resp.GetUploadUrl(),
			"method":     resp.GetMethod(),
			"expires_at": resp.GetExpiresAt(),
			"headers":    fiber.Map{"Content-Type": resp.GetAttachment().GetMimeType()},
		}
		return c.Status(fiber.StatusCreated).JSON(out)
	}
}

func completeAttachment(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		att, err := documents.CompleteUpload(forwardContext(c, claims),
			&documentsv1.CompleteUploadRequest{AttachmentId: c.Params("attachmentId")})
		if err != nil {
			return attachmentError(c, err)
		}
		if att.GetEntityId() != c.Params("id") {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "attachment not found", "code": "attachment_not_found"})
		}
		return c.JSON(attachmentJSON(att))
	}
}

func downloadAttachment(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := documents.GetDownloadUrl(forwardContext(c, claims),
			&documentsv1.GetDownloadUrlRequest{AttachmentId: c.Params("attachmentId")})
		if err != nil {
			return attachmentError(c, err)
		}
		if resp.GetAttachment().GetEntityId() != c.Params("id") {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "attachment not found", "code": "attachment_not_found"})
		}
		out := attachmentJSON(resp.GetAttachment())
		out["download"] = fiber.Map{"url": resp.GetDownloadUrl(), "expires_at": resp.GetExpiresAt()}
		return c.JSON(out)
	}
}

// listEntityAttachments powers the detail aggregations (vendor detail since
// #26, requisition detail since #33): metadata for every attachment of this
// owner, pending and ready.
func listEntityAttachments(ctx context.Context, entityType, entityID string, documents DocumentsClient) ([]fiber.Map, error) {
	resp, err := documents.ListAttachments(ctx, &documentsv1.ListAttachmentsRequest{
		EntityType: entityType, EntityId: entityID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]fiber.Map, 0, len(resp.GetAttachments()))
	for _, att := range resp.GetAttachments() {
		out = append(out, attachmentJSON(att))
	}
	return out, nil
}

// attachmentMessages renders the machine code (the gRPC message) as a
// human sentence; unknown messages pass through verbatim. attachment_entity_invalid
// is neutralized ("owner type", not "vendor") because the map is shared by
// the vendor and requisition attachment routes.
var attachmentMessages = map[string]string{
	"attachment_entity_invalid":    "attachment owner type is not supported",
	"attachment_filename_invalid":  "filename must be 1-255 characters without path separators",
	"attachment_mime_invalid":      "mime type is not allowed (pdf, png, jpeg, webp, docx, xlsx)",
	"attachment_too_large":         "file exceeds the size limit",
	"attachment_checksum_invalid":  "checksum must be a sha256 hex string",
	"attachment_id_invalid":        "attachment id is malformed",
	"attachment_not_found":         "attachment not found",
	"attachment_not_ready":         "attachment is still pending and not downloadable",
	"attachment_object_missing":    "uploaded file not found — upload it first",
	"attachment_size_mismatch":     "uploaded size differs from the declared size",
	"attachment_mime_mismatch":     "uploaded content type is not allowed",
	"attachment_checksum_mismatch": "uploaded content does not match the declared checksum",
}

// attachmentError maps documents' gRPC failures to HTTP with the machine
// code preserved (same shape as vendorError).
func attachmentError(c *fiber.Ctx, err error) error {
	st, _ := status.FromError(err)
	message, human := attachmentMessages[st.Message()]
	if !human {
		message = st.Message()
	}
	body := fiber.Map{"error": message, "code": st.Message()}
	switch st.Code() {
	case codes.InvalidArgument:
		return c.Status(fiber.StatusBadRequest).JSON(body)
	case codes.NotFound:
		return c.Status(fiber.StatusNotFound).JSON(body)
	case codes.AlreadyExists:
		return c.Status(fiber.StatusConflict).JSON(body)
	case codes.FailedPrecondition:
		return c.Status(fiber.StatusPreconditionFailed).JSON(body)
	case codes.Unauthenticated:
		return c.Status(fiber.StatusUnauthorized).JSON(body)
	case codes.PermissionDenied:
		return c.Status(fiber.StatusForbidden).JSON(body)
	default:
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "documents unavailable"})
	}
}

// attachmentJSON is the REST shape of attachment metadata; object_key stays
// internal (clients use the presigned URLs instead).
func attachmentJSON(a *documentsv1.Attachment) fiber.Map {
	return fiber.Map{
		"id":              a.GetId(),
		"tenant_id":       a.GetTenantId(),
		"entity_type":     a.GetEntityType(),
		"entity_id":       a.GetEntityId(),
		"filename":        a.GetFilename(),
		"mime_type":       a.GetMimeType(),
		"size_bytes":      a.GetSizeBytes(),
		"checksum_sha256": a.GetChecksumSha256(),
		"status":          a.GetStatus(),
		"created_by":      a.GetCreatedBy(),
		"created_at":      a.GetCreatedAt(),
		"uploaded_at":     a.GetUploadedAt(),
	}
}
