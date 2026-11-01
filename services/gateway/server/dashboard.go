package server

import (
	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerDashboardRoutes wires GET /api/v1/dashboard/summary (PRD §15, FR-09,
// issue #52): Bearer auth, the broad dashboard:read (every role — the landing
// page aggregates only reads those roles already hold, per docs/rbac.md), and
// the verified claims propagated to procurement as gRPC metadata. One route,
// one round trip: the summary envelope carries all three widgets.
func registerDashboardRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	dashboard := app.Group("/api/v1/dashboard", bearerAuth(verifier))
	dashboard.Get("/summary", RequirePermission(rbac.DashboardRead), getDashboardSummary(client))
}

// getDashboardSummary serves GET /api/v1/dashboard/summary: the tenant's
// operational-health snapshot — pending approvals (total + first page),
// spend per vendor over trailing 30/90-day windows, and the PO status
// distribution over the seven FR-06 statuses. No query parameters: the
// tenant is the token's, the windows are anchored server-side.
func getDashboardSummary(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		summary, err := client.GetDashboard(forwardContext(c, claims), &procurementv1.GetDashboardRequest{})
		if err != nil {
			return dashboardError(c, err)
		}
		return c.JSON(dashboardJSON(summary))
	}
}

// dashboardError renders the summary route's failures through the shared
// mapping. The read has no machine codes of its own (the request is empty —
// anonymous and under-privileged calls are answered by the middleware before
// this handler), so only the shared Internal/unavailable shape can surface.
func dashboardError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, dashboardMessages)
}

var dashboardMessages = map[string]string{}

// dashboardJSON renders the REST shape of the summary: pending_approvals
// {total, items}, spend_by_vendor [{vendor_id, vendor_name, currency,
// thirty_day_cents, ninety_day_cents}], po_status_distribution
// [{status, count}].
func dashboardJSON(s *procurementv1.DashboardSummary) fiber.Map {
	items := make([]fiber.Map, 0, len(s.GetPendingApprovals().GetItems()))
	for _, p := range s.GetPendingApprovals().GetItems() {
		items = append(items, fiber.Map{
			"requisition_id": p.GetRequisitionId(),
			"number":         p.GetNumber(),
			"currency":       p.GetCurrency(),
			"total_cents":    p.GetTotalCents(),
			"submitted_at":   p.GetSubmittedAt(),
			"step_id":        p.GetStepId(),
			"step_no":        p.GetStepNo(),
			"role_required":  p.GetRoleRequired(),
		})
	}
	spend := make([]fiber.Map, 0, len(s.GetSpendByVendor()))
	for _, v := range s.GetSpendByVendor() {
		spend = append(spend, fiber.Map{
			"vendor_id":        v.GetVendorId(),
			"vendor_name":      v.GetVendorName(),
			"currency":         v.GetCurrency(),
			"thirty_day_cents": v.GetThirtyDayCents(),
			"ninety_day_cents": v.GetNinetyDayCents(),
		})
	}
	dist := make([]fiber.Map, 0, len(s.GetPoStatusDistribution()))
	for _, d := range s.GetPoStatusDistribution() {
		dist = append(dist, fiber.Map{"status": d.GetStatus(), "count": d.GetCount()})
	}
	totals := make([]fiber.Map, 0, len(s.GetSpendTotals()))
	for _, c := range s.GetSpendTotals() {
		totals = append(totals, fiber.Map{
			"currency":         c.GetCurrency(),
			"thirty_day_cents": c.GetThirtyDayCents(),
			"ninety_day_cents": c.GetNinetyDayCents(),
		})
	}
	return fiber.Map{
		"pending_approvals": fiber.Map{
			"total": s.GetPendingApprovals().GetTotal(),
			"items": items,
		},
		"spend_by_vendor":        spend,
		"po_status_distribution": dist,
		"spend_totals":           totals,
	}
}
