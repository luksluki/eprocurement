package server

import (
	"context"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// VendorClient is the procurement surface the gateway proxies. The generated
// gRPC client satisfies it; tests substitute a fake.
type VendorClient interface {
	CreateVendor(ctx context.Context, in *procurementv1.CreateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error)
	GetVendor(ctx context.Context, in *procurementv1.GetVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error)
	UpdateVendor(ctx context.Context, in *procurementv1.UpdateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error)
	DeactivateVendor(ctx context.Context, in *procurementv1.DeactivateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error)
	ListVendors(ctx context.Context, in *procurementv1.ListVendorsRequest, opts ...grpc.CallOption) (*procurementv1.ListVendorsResponse, error)
}

// registerVendorRoutes wires /api/v1/vendors: Bearer auth, RBAC, and the
// verified claims propagated to procurement as gRPC metadata. When the
// documents client is present, attachment routes are added and vendor
// detail responses embed attachment metadata.
func registerVendorRoutes(app *fiber.App, verifier *token.Verifier, client VendorClient, documents DocumentsClient) {
	vendors := app.Group("/api/v1/vendors", bearerAuth(verifier))

	vendors.Get("/", RequirePermission(rbac.VendorRead), listVendors(client))
	vendors.Post("/", RequirePermission(rbac.VendorWrite), createVendor(client))
	vendors.Get("/:id", RequirePermission(rbac.VendorRead), getVendor(client, documents))
	vendors.Patch("/:id", RequirePermission(rbac.VendorWrite), updateVendor(client))
	vendors.Post("/:id/deactivate", RequirePermission(rbac.VendorWrite), deactivateVendor(client))
	if documents != nil {
		registerAttachmentRoutes(vendors, verifier, client, documents)
	}
}

// forwardContext attaches the verified claims as outgoing gRPC metadata —
// procurement scopes every query by this tenant and audits this user.
func forwardContext(c *fiber.Ctx, claims auth.AccessClaims) context.Context {
	return rbac.OutgoingContext(c.UserContext(), claims.Sub, claims.TenantID, claims.Roles)
}

func claimsOf(c *fiber.Ctx) (auth.AccessClaims, bool) {
	claims, ok := c.Locals("claims").(auth.AccessClaims)
	return claims, ok
}

type vendorBody struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactEmail string `json:"contact_email"`
	TaxID        string `json:"tax_id"`
}

func createVendor(client VendorClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body vendorBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		v, err := client.CreateVendor(forwardContext(c, claims), &procurementv1.CreateVendorRequest{
			Code:         strings.ToUpper(strings.TrimSpace(body.Code)),
			Name:         strings.TrimSpace(body.Name),
			ContactEmail: strings.TrimSpace(body.ContactEmail),
			TaxId:        strings.TrimSpace(body.TaxID),
		})
		if err != nil {
			return vendorError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(vendorJSON(v))
	}
}

func getVendor(client VendorClient, documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		v, err := client.GetVendor(forwardContext(c, claims), &procurementv1.GetVendorRequest{Id: c.Params("id")})
		if err != nil {
			return vendorError(c, err)
		}
		out := vendorJSON(v)
		if documents != nil {
			attachments, err := listVendorAttachments(forwardContext(c, claims), v.GetId(), documents)
			if err != nil {
				return attachmentError(c, err)
			}
			out["attachments"] = attachments
		}
		return c.JSON(out)
	}
}

func updateVendor(client VendorClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body vendorBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		v, err := client.UpdateVendor(forwardContext(c, claims), &procurementv1.UpdateVendorRequest{
			Id:           c.Params("id"),
			Name:         strings.TrimSpace(body.Name),
			ContactEmail: strings.TrimSpace(body.ContactEmail),
			TaxId:        strings.TrimSpace(body.TaxID),
		})
		if err != nil {
			return vendorError(c, err)
		}
		return c.JSON(vendorJSON(v))
	}
}

func deactivateVendor(client VendorClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		v, err := client.DeactivateVendor(forwardContext(c, claims),
			&procurementv1.DeactivateVendorRequest{Id: c.Params("id")})
		if err != nil {
			return vendorError(c, err)
		}
		return c.JSON(vendorJSON(v))
	}
}

// vendorMessages renders the machine code (the gRPC message) as a human
// sentence; unknown messages pass through verbatim.
var vendorMessages = map[string]string{
	"vendor_duplicate_code": "a vendor with this code already exists",
	"vendor_not_found":      "vendor not found",
	"vendor_deactivated":    "vendor is deactivated",
	"vendor_code_invalid":   "vendor code must be 2-32 chars of A-Z, 0-9, -",
	"vendor_name_invalid":   "vendor name must be 2-200 characters",
	"vendor_email_invalid":  "contact email is not a valid address",
	"vendor_status_invalid": "status must be active, deactivated, or empty",
	"vendor_page_invalid":   "page is out of range",
}

// vendorError maps procurement's gRPC failures to HTTP with the machine code
// preserved: InvalidArgument→400, NotFound→404, AlreadyExists→409,
// FailedPrecondition→412, Unauthenticated→401, everything else→502.
func vendorError(c *fiber.Ctx, err error) error {
	st, _ := status.FromError(err)
	message, human := vendorMessages[st.Message()]
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
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "procurement unavailable"})
	}
}

func vendorJSON(v *procurementv1.Vendor) fiber.Map {
	return fiber.Map{
		"id":            v.GetId(),
		"tenant_id":     v.GetTenantId(),
		"code":          v.GetCode(),
		"name":          v.GetName(),
		"contact_email": v.GetContactEmail(),
		"tax_id":        v.GetTaxId(),
		"status":        v.GetStatus(),
		"created_at":    v.GetCreatedAt(),
		"created_by":    v.GetCreatedBy(),
		"updated_at":    v.GetUpdatedAt(),
		"updated_by":    v.GetUpdatedBy(),
	}
}

// listVendors serves GET /api/v1/vendors: query params q (code prefix or
// name substring), status, page, page_size — normalized and validated in
// procurement.
func listVendors(client VendorClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		page, _ := strconv.Atoi(c.Query("page"))
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		resp, err := client.ListVendors(forwardContext(c, claims), &procurementv1.ListVendorsRequest{
			Query:    strings.TrimSpace(c.Query("q")),
			Status:   strings.TrimSpace(c.Query("status")),
			Page:     int32(page),
			PageSize: int32(pageSize),
		})
		if err != nil {
			return vendorError(c, err)
		}
		out := make([]fiber.Map, 0, len(resp.GetVendors()))
		for _, v := range resp.GetVendors() {
			out = append(out, vendorJSON(v))
		}
		return c.JSON(fiber.Map{
			"vendors":   out,
			"total":     resp.GetTotal(),
			"page":      resp.GetPage(),
			"page_size": resp.GetPageSize(),
		})
	}
}
