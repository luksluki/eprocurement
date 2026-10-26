package server

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerPurchaseOrderRoutes wires /api/v1/purchase-orders: Bearer auth,
// RBAC (po:write — buyer/admin at both layers; docs/rbac.md), and the
// verified claims propagated to procurement as gRPC metadata. #35 lands the
// conversion endpoint only; PRD §15's issue and goods-receipt routes belong
// to #36/#37.
func registerPurchaseOrderRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	pos := app.Group("/api/v1/purchase-orders", bearerAuth(verifier))

	pos.Post("/from-requisition/:id", RequirePermission(rbac.POWrite), createPurchaseOrder(client))
}

// poBody is the conversion payload. vendor_id is required — the requisition
// carries no vendor (PRD §13); it must name an active vendor of the tenant
// (procurement validates the UUID and the active gate: 400
// po_vendor_id_invalid, 412 vendor_deactivated).
type poBody struct {
	VendorID string `json:"vendor_id"`
}

// createPurchaseOrder serves POST /api/v1/purchase-orders/from-requisition/{id}
// (PRD §15): converts an approved requisition into a draft PO. Idempotent:
// converting the same requisition again returns the existing PO unchanged —
// 201 when this call created it, 200 when it replays the existing one (the
// created flag from procurement draws the line).
func createPurchaseOrder(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body poBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		resp, err := client.CreatePurchaseOrder(forwardContext(c, claims), &procurementv1.CreatePurchaseOrderRequest{
			RequisitionId: c.Params("id"),
			VendorId:      strings.TrimSpace(body.VendorID),
		})
		if err != nil {
			return poError(c, err)
		}
		status := fiber.StatusOK
		if resp.GetCreated() {
			status = fiber.StatusCreated
		}
		return c.Status(status).JSON(purchaseOrderJSON(resp.GetPurchaseOrder()))
	}
}

// poMessages renders each machine code (the gRPC message) as a human
// sentence; the requisition/vendor gate codes ride along because the
// conversion crosses those domains.
var poMessages = map[string]string{
	"po_vendor_id_invalid":        "vendor_id must be the UUID of the vendor to order from",
	"po_requisition_not_approved": "only an approved requisition can be converted",
	"po_currency_invalid":         "currency must be IDR or USD",
	"po_items_empty":              "requisition has no items to copy",
	"requisition_not_found":       "requisition not found",
	"requisition_id_invalid":      "requisition id must be a UUID",
	"vendor_not_found":            "vendor not found",
	"vendor_deactivated":          "vendor is deactivated",
}

// poError renders procurement's purchase-order failures through the shared
// gRPC→HTTP mapping with the machine code preserved.
func poError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, poMessages)
}

func purchaseOrderJSON(po *procurementv1.PurchaseOrder) fiber.Map {
	items := make([]fiber.Map, 0, len(po.GetItems()))
	for _, it := range po.GetItems() {
		items = append(items, fiber.Map{
			"description":      it.GetDescription(),
			"qty_ordered":      it.GetQtyOrdered(),
			"qty_received":     it.GetQtyReceived(),
			"unit_price_cents": it.GetUnitPriceCents(),
		})
	}
	return fiber.Map{
		"id":             po.GetId(),
		"tenant_id":      po.GetTenantId(),
		"number":         po.GetNumber(),
		"requisition_id": po.GetRequisitionId(),
		"vendor_id":      po.GetVendorId(),
		"status":         po.GetStatus(),
		"currency":       po.GetCurrency(),
		"total_cents":    po.GetTotalCents(),
		"items":          items,
		"issued_at":      po.GetIssuedAt(),
		"created_at":     po.GetCreatedAt(),
		"created_by":     po.GetCreatedBy(),
		"updated_at":     po.GetUpdatedAt(),
		"updated_by":     po.GetUpdatedBy(),
	}
}
