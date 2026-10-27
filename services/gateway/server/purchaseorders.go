package server

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerPurchaseOrderRoutes wires /api/v1/purchase-orders: Bearer auth,
// RBAC, and the verified claims propagated to procurement as gRPC metadata.
// #35 landed the conversion endpoint; #36 the lifecycle actions (PRD §15
// issue route plus acknowledge/cancel/close); #37 the goods-receipt posting
// route (PRD §15, grn:post — buyer/admin; see goodsreceipt.go).
func registerPurchaseOrderRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	pos := app.Group("/api/v1/purchase-orders", bearerAuth(verifier))

	pos.Post("/from-requisition/:id", RequirePermission(rbac.POWrite), createPurchaseOrder(client))
	pos.Post("/:id/issue", RequirePermission(rbac.POWrite), issuePurchaseOrder(client))
	pos.Post("/:id/acknowledge", RequirePermission(rbac.POWrite), acknowledgePurchaseOrder(client))
	pos.Post("/:id/cancel", RequirePermission(rbac.POWrite), cancelPurchaseOrder(client))
	pos.Post("/:id/close", RequirePermission(rbac.POWrite), closePurchaseOrder(client))
	pos.Post("/:id/goods-receipts", RequirePermission(rbac.GRNPost), postGoodsReceiptRoute(client))
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
// conversion crosses those domains, and the lifecycle guards join with #36.
var poMessages = map[string]string{
	"po_vendor_id_invalid":        "vendor_id must be the UUID of the vendor to order from",
	"po_requisition_not_approved": "only an approved requisition can be converted",
	"po_currency_invalid":         "currency must be IDR or USD",
	"po_items_empty":              "requisition has no items to copy",
	"po_id_invalid":               "purchase order id must be a UUID",
	"po_illegal_transition":       "the purchase order cannot move that way from its current status",
	"po_has_receipts":             "goods were already received; close the purchase order instead of cancelling",
	"requisition_not_found":       "requisition not found",
	"requisition_id_invalid":      "requisition id must be a UUID",
	"vendor_not_found":            "vendor not found",
	"vendor_deactivated":          "vendor is deactivated",
	"po_not_found":                "purchase order not found",
}

// poError renders procurement's purchase-order failures through the shared
// gRPC→HTTP mapping with the machine code preserved.
func poError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, poMessages)
}

// poAction is the shared body of the four lifecycle routes (#36): the path
// id names the PO, the request carries no body, and the updated PO answers
// 200. Errors flow through poError — po_id_invalid → 400, po_not_found →
// 404, po_illegal_transition / po_has_receipts → 409 (FR-06).
func poAction(call func(context.Context, string) (*procurementv1.PurchaseOrder, error)) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		po, err := call(forwardContext(c, claims), c.Params("id"))
		if err != nil {
			return poError(c, err)
		}
		return c.JSON(purchaseOrderJSON(po))
	}
}

// issuePurchaseOrder serves POST /api/v1/purchase-orders/{id}/issue (PRD
// §15): draft → issued, issued_at stamped.
func issuePurchaseOrder(client ProcurementClient) fiber.Handler {
	return poAction(func(ctx context.Context, id string) (*procurementv1.PurchaseOrder, error) {
		return client.IssuePurchaseOrder(ctx, &procurementv1.IssuePurchaseOrderRequest{Id: id})
	})
}

// acknowledgePurchaseOrder serves POST /api/v1/purchase-orders/{id}/acknowledge:
// issued → acknowledged (the vendor confirmation).
func acknowledgePurchaseOrder(client ProcurementClient) fiber.Handler {
	return poAction(func(ctx context.Context, id string) (*procurementv1.PurchaseOrder, error) {
		return client.AcknowledgePurchaseOrder(ctx, &procurementv1.AcknowledgePurchaseOrderRequest{Id: id})
	})
}

// cancelPurchaseOrder serves POST /api/v1/purchase-orders/{id}/cancel:
// draft/issued/acknowledged → cancelled, blocked once anything was received.
func cancelPurchaseOrder(client ProcurementClient) fiber.Handler {
	return poAction(func(ctx context.Context, id string) (*procurementv1.PurchaseOrder, error) {
		return client.CancelPurchaseOrder(ctx, &procurementv1.CancelPurchaseOrderRequest{Id: id})
	})
}

// closePurchaseOrder serves POST /api/v1/purchase-orders/{id}/close:
// partially_received (short-close) or received → closed.
func closePurchaseOrder(client ProcurementClient) fiber.Handler {
	return poAction(func(ctx context.Context, id string) (*procurementv1.PurchaseOrder, error) {
		return client.ClosePurchaseOrder(ctx, &procurementv1.ClosePurchaseOrderRequest{Id: id})
	})
}

func purchaseOrderJSON(po *procurementv1.PurchaseOrder) fiber.Map {
	items := make([]fiber.Map, 0, len(po.GetItems()))
	for _, it := range po.GetItems() {
		items = append(items, fiber.Map{
			"id":               it.GetId(),
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
