package server

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
)

// postGoodsReceiptRoute serves POST /api/v1/purchase-orders/{id}/goods-receipts
// (PRD §15, FR-07): records one (partial or full) goods receipt against the
// path PO. Body: {"items": [{"po_item_id", "qty_received"}], "notes"} — the
// path id names the PO, po_item_id names its lines (exposed as item ids in
// the PO responses since #37). 201 + the stored GRN with the updated PO
// embedded on success.
func postGoodsReceiptRoute(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body grnBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		items := make([]*procurementv1.GoodsReceiptItem, 0, len(body.Items))
		for _, it := range body.Items {
			items = append(items, &procurementv1.GoodsReceiptItem{
				PoItemId:    strings.TrimSpace(it.POItemID),
				QtyReceived: it.QtyReceived,
			})
		}
		resp, err := client.PostGoodsReceipt(forwardContext(c, claims), &procurementv1.PostGoodsReceiptRequest{
			PoId:  c.Params("id"),
			Items: items,
			Notes: strings.TrimSpace(body.Notes),
		})
		if err != nil {
			return grnError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(goodsReceiptJSON(resp))
	}
}

// grnItemBody is one receipt line of the request body.
type grnItemBody struct {
	POItemID    string `json:"po_item_id"`
	QtyReceived int32  `json:"qty_received"`
}

// grnBody is the posting payload: at least one line (validated in
// procurement) and optional notes.
type grnBody struct {
	Items []grnItemBody `json:"items"`
	Notes string        `json:"notes"`
}

// grnError renders the goods-receipt route's failures. The over-receipt
// answer is the one procurement error whose gRPC message carries detail
// beyond the machine code ("grn_over_receipt: po_item=… received=…
// requested=… ordered=…" — the format pinned in docs/schema/procurement.md),
// so this route deliberately does not flatten the message through the shared
// messages map: the body's code field is the token before the first colon —
// exactly "grn_over_receipt", stable for clients that match on equality —
// while error carries the full detailed message; the status follows the
// shared Aborted → 409 mapping. Every other code on this route flows through
// the shared grpcHTTPError unchanged.
func grnError(c *fiber.Ctx, err error) error {
	st, _ := status.FromError(err)
	if msg := st.Message(); st.Code() == codes.Aborted && strings.HasPrefix(msg, overReceiptPrefix) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": msg,
			"code":  strings.TrimSuffix(overReceiptPrefix, ":"),
		})
	}
	return grpcHTTPError(c, err, grnMessages)
}

// overReceiptPrefix is the strict machine-code prefix of the pinned
// over-receipt detail line; the token before the first colon.
const overReceiptPrefix = "grn_over_receipt:"

// grnMessages renders each machine code (the gRPC message) as a human
// sentence; the PO gate codes ride along because a receipt crosses the
// purchase-order aggregate.
var grnMessages = map[string]string{
	"grn_items_empty":       "a goods receipt needs at least one item line",
	"grn_qty_invalid":       "each received quantity must be greater than zero",
	"grn_po_item_duplicate": "each purchase order line may appear once per goods receipt",
	"grn_notes_invalid":     "notes are limited to 1000 characters",
	"grn_po_item_unknown":   "po_item_id does not name a line of this purchase order",
	"po_id_invalid":         "purchase order id must be a UUID",
	"po_not_found":          "purchase order not found",
	"po_illegal_transition": "goods can only be received on an acknowledged or partially received purchase order",
}

// goodsReceiptJSON renders the posting response: the GRN with the updated
// PO embedded (its lines' qty_received advanced, status moved).
func goodsReceiptJSON(resp *procurementv1.PostGoodsReceiptResponse) fiber.Map {
	grn := resp.GetGoodsReceipt()
	items := make([]fiber.Map, 0, len(grn.GetItems()))
	for _, it := range grn.GetItems() {
		items = append(items, fiber.Map{
			"po_item_id":   it.GetPoItemId(),
			"qty_received": it.GetQtyReceived(),
		})
	}
	return fiber.Map{
		"id":             grn.GetId(),
		"tenant_id":      grn.GetTenantId(),
		"po_id":          grn.GetPoId(),
		"number":         grn.GetNumber(),
		"received_by":    grn.GetReceivedBy(),
		"received_at":    grn.GetReceivedAt(),
		"notes":          grn.GetNotes(),
		"items":          items,
		"created_at":     grn.GetCreatedAt(),
		"purchase_order": purchaseOrderJSON(resp.GetPurchaseOrder()),
	}
}
