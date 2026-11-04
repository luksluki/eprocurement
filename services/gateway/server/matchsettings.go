package server

import (
	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerMatchSettingsRoutes wires the admin-only tolerance surface
// (#40, FR-08 "configurable tolerance"): GET|PUT /api/v1/match-settings
// behind match:configure — the approval:configure twin, admin-only at the
// edge and re-checked by the procurement interceptor (maintainer decision
// 2026-10-28).
func registerMatchSettingsRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	settings := app.Group("/api/v1/match-settings", bearerAuth(verifier))
	settings.Get("/", RequirePermission(rbac.MatchConfigure), getMatchSettings(client))
	settings.Put("/", RequirePermission(rbac.MatchConfigure), replaceMatchSettings(client))
}

// matchSettingsBody is the settings payload both routes share: whole
// percentages, 0..100 each (procurement validates and answers the machine
// codes match_qty_pct_invalid / match_price_pct_invalid).
type matchSettingsBody struct {
	QtyPct   int32 `json:"qty_pct"`
	PricePct int32 `json:"price_pct"`
}

// getMatchSettings serves GET /api/v1/match-settings (admin-only): the
// tenant's tolerances, or the effective 0%/0% defaults while no row is
// stored.
func getMatchSettings(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := client.GetMatchSettings(forwardContext(c, claims), &procurementv1.GetMatchSettingsRequest{})
		if err != nil {
			return matchSettingsError(c, err)
		}
		return c.JSON(matchSettingsJSON(resp))
	}
}

// replaceMatchSettings serves PUT /api/v1/match-settings (admin-only): the
// tenant's complete new tolerances.
func replaceMatchSettings(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body matchSettingsBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		resp, err := client.ReplaceMatchSettings(forwardContext(c, claims), &procurementv1.ReplaceMatchSettingsRequest{
			QtyPct: body.QtyPct, PricePct: body.PricePct,
		})
		if err != nil {
			return matchSettingsError(c, err)
		}
		return c.JSON(matchSettingsJSON(resp))
	}
}

func matchSettingsJSON(s *procurementv1.MatchSettings) fiber.Map {
	return fiber.Map{"qty_pct": s.GetQtyPct(), "price_pct": s.GetPricePct()}
}

// matchSettingsMessages renders each settings machine code as a human
// sentence.
var matchSettingsMessages = map[string]string{
	"match_qty_pct_invalid":   "quantity tolerance must be a whole percentage between 0 and 100",
	"match_price_pct_invalid": "price tolerance must be a whole percentage between 0 and 100",
}

// matchSettingsError renders procurement's settings failures through the
// shared gRPC→HTTP mapping with the machine code preserved.
func matchSettingsError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, matchSettingsMessages)
}
