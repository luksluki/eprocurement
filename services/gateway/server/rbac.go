package server

import (
	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// RequirePermission is the gateway's RBAC middleware. It runs after
// bearerAuth, which stored the verified claims; a caller whose roles do not
// grant the permission gets 403 with a machine-readable reason code.
func RequirePermission(permission string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := c.Locals("claims").(auth.AccessClaims)
		if !ok {
			// Reached only when route registration skipped bearerAuth.
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		if !rbac.Check(claims.Roles, permission) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":    "forbidden",
				"code":     "permission_denied",
				"required": permission,
			})
		}
		return c.Next()
	}
}
