package server

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerInvoiceRoutes wires /api/v1/invoices: Bearer auth, RBAC (writes:
// invoice:write — finance/admin at both layers; reads: invoice:read —
// everyone in the tenant), and the verified claims propagated to
// procurement as gRPC metadata. When documents is present, GET /:id embeds
// attachment metadata and the attachment routes are registered (maintainer
// decision 2026-10-27: attachment support ships with #39; the "required"
// obligation is the demo flow's, #41/#40 — the create API never demands
// one).
func registerInvoiceRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient, documents DocumentsClient) {
	invoices := app.Group("/api/v1/invoices", bearerAuth(verifier))

	invoices.Post("/", RequirePermission(rbac.InvoiceWrite), createInvoice(client))
	invoices.Get("/:id", RequirePermission(rbac.InvoiceRead), getInvoice(client, documents))
	if documents != nil {
		registerInvoiceAttachmentRoutes(invoices, verifier, client, documents)
	}
}

// invoiceItemBody is one invoice line of the request body.
type invoiceItemBody struct {
	POItemID       string `json:"po_item_id"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

// invoiceBody is the create payload: the PO to bill, the vendor's own
// invoice number, and the lines. vendor_id, currency, and the total are
// derived server-side in procurement — the client never sends them.
type invoiceBody struct {
	POID   string            `json:"po_id"`
	Number string            `json:"number"`
	Items  []invoiceItemBody `json:"items"`
}

// createInvoice serves POST /api/v1/invoices (PRD §15): records one vendor
// invoice against a PO of the caller's tenant. 201 + the stored invoice on
// success.
func createInvoice(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body invoiceBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		items := make([]*procurementv1.InvoiceItem, 0, len(body.Items))
		for _, it := range body.Items {
			items = append(items, &procurementv1.InvoiceItem{
				PoItemId:       strings.TrimSpace(it.POItemID),
				Qty:            it.Qty,
				UnitPriceCents: it.UnitPriceCents,
			})
		}
		inv, err := client.CreateInvoice(forwardContext(c, claims), &procurementv1.CreateInvoiceRequest{
			PoId:   strings.TrimSpace(body.POID),
			Number: strings.TrimSpace(body.Number),
			Items:  items,
		})
		if err != nil {
			return invoiceError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(invoiceJSON(inv))
	}
}

// getInvoice serves GET /api/v1/invoices/:id — the detail read (header plus
// lines, match_result_json while pending is empty). When documents is
// wired, attachment metadata is embedded (the vendor/requisition detail
// parity) so the UI renders the invoice's documents without a second round
// trip; without documents the key is absent. A documents failure fails the
// whole read rather than silently dropping attachments.
func getInvoice(client ProcurementClient, documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		inv, err := client.GetInvoice(forwardContext(c, claims), &procurementv1.GetInvoiceRequest{Id: c.Params("id")})
		if err != nil {
			return invoiceError(c, err)
		}
		out := invoiceJSON(inv)
		if documents != nil {
			attachments, err := listEntityAttachments(forwardContext(c, claims), entityInvoice, inv.GetId(), documents)
			if err != nil {
				return attachmentError(c, err)
			}
			out["attachments"] = attachments
		}
		return c.JSON(out)
	}
}

// invoiceError renders the invoice routes' failures. The over-billing
// answer is this family's one procurement error whose gRPC message carries
// detail beyond the machine code ("inv_over_bill: po_number=…
// invoiced=… requested=… total=…" — the format pinned in
// docs/schema/procurement.md, the grn_over_receipt twin of #37), so this
// renderer deliberately does not flatten the message through the shared
// messages map: the body's code field is the token before the first colon —
// exactly "inv_over_bill", stable for clients that match on equality —
// while error carries the full detailed message; the status follows the
// shared Aborted → 409 mapping. Every other code on these routes flows
// through the shared grpcHTTPError unchanged.
func invoiceError(c *fiber.Ctx, err error) error {
	st, _ := status.FromError(err)
	if msg := st.Message(); st.Code() == codes.Aborted && strings.HasPrefix(msg, overBillPrefix) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": msg,
			"code":  strings.TrimSuffix(overBillPrefix, ":"),
		})
	}
	return grpcHTTPError(c, err, invoiceMessages)
}

// overBillPrefix is the strict machine-code prefix of the pinned
// over-billing detail line; the token before the first colon.
const overBillPrefix = "inv_over_bill:"

// invoiceMessages renders each machine code (the gRPC message) as a human
// sentence; the PO gate codes ride along because invoicing crosses the
// purchase-order aggregate.
var invoiceMessages = map[string]string{
	"inv_number_invalid":     "invoice number must be 1-100 characters after trimming",
	"inv_items_empty":        "an invoice needs at least one item line",
	"inv_qty_invalid":        "each invoiced quantity must be greater than zero",
	"inv_unit_price_invalid": "each invoiced unit price must be zero or more",
	"inv_po_item_duplicate":  "each purchase order line may appear once per invoice",
	"inv_po_item_unknown":    "po_item_id does not name a line of this purchase order",
	"inv_id_invalid":         "invoice id must be a UUID",
	"inv_number_duplicate":   "this vendor already has an invoice with this number",
	"inv_not_found":          "invoice not found",
	"po_id_invalid":          "purchase order id must be a UUID",
	"po_not_found":           "purchase order not found",
	"po_illegal_transition":  "only acknowledged, partially received, or received purchase orders can be invoiced",
}

// invoiceJSON renders the REST shape of one invoice.
func invoiceJSON(inv *procurementv1.Invoice) fiber.Map {
	items := make([]fiber.Map, 0, len(inv.GetItems()))
	for _, it := range inv.GetItems() {
		items = append(items, fiber.Map{
			"po_item_id":       it.GetPoItemId(),
			"qty":              it.GetQty(),
			"unit_price_cents": it.GetUnitPriceCents(),
		})
	}
	return fiber.Map{
		"id":                inv.GetId(),
		"tenant_id":         inv.GetTenantId(),
		"po_id":             inv.GetPoId(),
		"vendor_id":         inv.GetVendorId(),
		"number":            inv.GetNumber(),
		"currency":          inv.GetCurrency(),
		"total_cents":       inv.GetTotalCents(),
		"status":            inv.GetStatus(),
		"match_result_json": inv.GetMatchResultJson(),
		"items":             items,
		"created_by":        inv.GetCreatedBy(),
		"created_at":        inv.GetCreatedAt(),
	}
}
