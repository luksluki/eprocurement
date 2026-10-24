package server

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerRequisitionRoutes wires /api/v1/requisitions: Bearer auth, RBAC
// (writes: requisition:write — requester/admin at both layers; list:
// requisition:read — everyone in the tenant), and the verified claims
// propagated to procurement as gRPC metadata. The requester-ownership rule
// on edit/submit is enforced by procurement (docs/rbac.md).
func registerRequisitionRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	reqs := app.Group("/api/v1/requisitions", bearerAuth(verifier))

	reqs.Get("/", RequirePermission(rbac.RequisitionRead), listRequisitions(client))
	reqs.Post("/", RequirePermission(rbac.RequisitionWrite), createRequisition(client))
	reqs.Put("/:id", RequirePermission(rbac.RequisitionWrite), updateRequisition(client))
	reqs.Post("/:id/submit", RequirePermission(rbac.RequisitionWrite), submitRequisition(client))
}

// listRequisitions serves GET /api/v1/requisitions: query params q (number
// prefix), status, page, page_size — normalized and validated in procurement.
// The response envelope is {requisitions, total, page, page_size}; rows are
// header-only (items/approval_steps empty).
func listRequisitions(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		page, _ := strconv.Atoi(c.Query("page"))
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		resp, err := client.ListRequisitions(forwardContext(c, claims), &procurementv1.ListRequisitionsRequest{
			Query:    strings.TrimSpace(c.Query("q")),
			Status:   strings.TrimSpace(c.Query("status")),
			Page:     int32(page),
			PageSize: int32(pageSize),
		})
		if err != nil {
			return requisitionError(c, err)
		}
		out := make([]fiber.Map, 0, len(resp.GetRequisitions()))
		for _, r := range resp.GetRequisitions() {
			out = append(out, requisitionJSON(r))
		}
		return c.JSON(fiber.Map{
			"requisitions": out,
			"total":        resp.GetTotal(),
			"page":         resp.GetPage(),
			"page_size":    resp.GetPageSize(),
		})
	}
}

// The client payload carries no totals and no number: both are computed
// server-side and echoed back in the response.
type requisitionItemBody struct {
	Description    string `json:"description"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	Category       string `json:"category"`
}

type requisitionBody struct {
	Currency string                `json:"currency"`
	Items    []requisitionItemBody `json:"items"`
}

func itemPB(items []requisitionItemBody) []*procurementv1.RequisitionItem {
	out := make([]*procurementv1.RequisitionItem, 0, len(items))
	for _, it := range items {
		out = append(out, &procurementv1.RequisitionItem{
			Description:    strings.TrimSpace(it.Description),
			Qty:            it.Qty,
			UnitPriceCents: it.UnitPriceCents,
			Category:       strings.TrimSpace(it.Category),
		})
	}
	return out
}

// createRequisition serves POST /api/v1/requisitions: the caller becomes the
// requester; currency is normalized (trimmed, uppercased) at the edge and is
// required — the domain rejects empty/unknown values with 400, there is no
// default.
func createRequisition(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body requisitionBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		r, err := client.CreateRequisition(forwardContext(c, claims), &procurementv1.CreateRequisitionRequest{
			Currency: strings.ToUpper(strings.TrimSpace(body.Currency)),
			Items:    itemPB(body.Items),
		})
		if err != nil {
			return requisitionError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(requisitionJSON(r))
	}
}

// updateRequisition serves PUT /api/v1/requisitions/{id}: replaces the whole
// basket of a draft the caller owns and returns the recomputed total.
func updateRequisition(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body requisitionBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		r, err := client.UpdateRequisition(forwardContext(c, claims), &procurementv1.UpdateRequisitionRequest{
			Id:    c.Params("id"),
			Items: itemPB(body.Items),
		})
		if err != nil {
			return requisitionError(c, err)
		}
		return c.JSON(requisitionJSON(r))
	}
}

// submitRequisition serves POST /api/v1/requisitions/{id}/submit. The
// optional Idempotency-Key header (1–64 chars after trimming) names the
// submit: replaying the same key returns the requisition's current state
// instead of a second transition or a 409.
func submitRequisition(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		key := strings.TrimSpace(c.Get("Idempotency-Key"))
		if len(key) > 64 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Idempotency-Key must be 1-64 characters",
				"code":  "requisition_idempotency_key_invalid",
			})
		}
		r, err := client.SubmitRequisition(forwardContext(c, claims), &procurementv1.SubmitRequisitionRequest{
			Id:             c.Params("id"),
			IdempotencyKey: key,
		})
		if err != nil {
			return requisitionError(c, err)
		}
		return c.JSON(requisitionJSON(r))
	}
}

// requisitionMessages renders each machine code as a human sentence.
var requisitionMessages = map[string]string{
	"requisition_not_found":                "requisition not found",
	"requisition_forbidden":                "only the requester (or an admin) may modify this requisition",
	"requisition_id_invalid":               "requisition id must be a UUID",
	"requisition_currency_invalid":         "currency must be IDR or USD",
	"requisition_description_invalid":      "item description must not be blank",
	"requisition_qty_invalid":              "item qty must be greater than zero",
	"requisition_unit_price_invalid":       "item unit price must be zero or more",
	"requisition_items_empty":              "add at least one item before submitting",
	"requisition_illegal_transition":       "requisition is no longer a draft",
	"requisition_idempotency_key_invalid":  "Idempotency-Key must be 1-64 characters",
	"requisition_idempotency_key_conflict": "Idempotency-Key was already used for another requisition",
	"requisition_status_invalid":           "status must be a requisition status or empty",
	"requisition_page_invalid":             "page is out of range",
}

// requisitionError renders procurement's requisition failures through the
// shared gRPC→HTTP mapping with the machine code preserved.
func requisitionError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, requisitionMessages)
}

func requisitionJSON(r *procurementv1.Requisition) fiber.Map {
	items := make([]fiber.Map, 0, len(r.GetItems()))
	for _, it := range r.GetItems() {
		items = append(items, fiber.Map{
			"description":      it.GetDescription(),
			"qty":              it.GetQty(),
			"unit_price_cents": it.GetUnitPriceCents(),
			"category":         it.GetCategory(),
		})
	}
	steps := make([]fiber.Map, 0, len(r.GetApprovalSteps()))
	for _, s := range r.GetApprovalSteps() {
		steps = append(steps, fiber.Map{
			"id":              s.GetId(),
			"requisition_id":  s.GetRequisitionId(),
			"step_no":         s.GetStepNo(),
			"role_required":   s.GetRoleRequired(),
			"threshold_cents": s.GetThresholdCents(),
			"status":          s.GetStatus(),
			"decided_by":      s.GetDecidedBy(),
			"decided_at":      s.GetDecidedAt(),
			"reason":          s.GetReason(),
		})
	}
	return fiber.Map{
		"id":             r.GetId(),
		"tenant_id":      r.GetTenantId(),
		"number":         r.GetNumber(),
		"requester_id":   r.GetRequesterId(),
		"status":         r.GetStatus(),
		"currency":       r.GetCurrency(),
		"total_cents":    r.GetTotalCents(),
		"items":          items,
		"approval_steps": steps,
		"submitted_at":   r.GetSubmittedAt(),
		"created_at":     r.GetCreatedAt(),
		"created_by":     r.GetCreatedBy(),
		"updated_at":     r.GetUpdatedAt(),
		"updated_by":     r.GetUpdatedBy(),
	}
}
