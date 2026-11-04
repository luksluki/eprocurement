package server

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// tokenFor mints an access token carrying exactly the given roles.
func tokenFor(t *testing.T, priv ed25519.PrivateKey, kid string, roles []string) string {
	t.Helper()
	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Iss: "identity.eprocurement", Sub: "u", TenantID: "t",
		Roles: roles, JTI: "j", IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// routePath turns "vendor:write" into "/test/vendor/write": fiber paths must
// not contain ":" (parameter syntax), so the permission's area/action split
// becomes the path hierarchy.
func routePath(p string) string {
	return "/test/" + strings.ReplaceAll(p, ":", "/")
}

// AC: each role tested against a route matrix — every role × every permission
// through the real fiber stack (bearerAuth + RequirePermission), asserting
// both the status code and the 403 reason body.
func TestRouteMatrixAllRolesAllPermissions(t *testing.T) {
	app, verifier, priv, kid := newTestApp(t)

	// Register one route per permission on the shared app.
	for _, p := range rbac.AllPermissions() {
		app.Get(routePath(p), bearerAuth(verifier), RequirePermission(p), func(c *fiber.Ctx) error {
			return c.JSON(fiber.Map{"ok": true})
		})
	}

	for _, role := range []string{
		rbac.RoleAdmin, rbac.RoleRequester, rbac.RoleApprover,
		rbac.RoleBuyer, rbac.RoleFinance, rbac.RoleViewer,
	} {
		token := tokenFor(t, priv, kid, []string{role})
		for _, p := range rbac.AllPermissions() {
			resp, body := do(t, app, "GET", routePath(p), "Bearer "+token)
			if rbac.Check([]string{role}, p) {
				if resp.StatusCode != fiber.StatusOK {
					t.Errorf("%s GET %s: status = %d, want 200 (body %v)", role, p, resp.StatusCode, body)
				}
				continue
			}
			if resp.StatusCode != fiber.StatusForbidden {
				t.Errorf("%s GET %s: status = %d, want 403", role, p, resp.StatusCode)
				continue
			}
			if body["code"] != "permission_denied" || body["required"] != p {
				t.Errorf("%s GET %s: 403 body = %v, want code=permission_denied required=%s", role, p, body, p)
			}
		}
	}
}

func TestRequirePermissionMultiRoleToken(t *testing.T) {
	app, verifier, priv, kid := newTestApp(t)
	app.Get(routePath(rbac.VendorWrite), bearerAuth(verifier), RequirePermission(rbac.VendorWrite),
		func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	// viewer alone cannot write vendors; viewer+buyer can (any granting role).
	limited := tokenFor(t, priv, kid, []string{rbac.RoleViewer})
	resp, _ := do(t, app, "GET", routePath(rbac.VendorWrite), "Bearer "+limited)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", resp.StatusCode)
	}

	mixed := tokenFor(t, priv, kid, []string{rbac.RoleViewer, rbac.RoleBuyer})
	resp, _ = do(t, app, "GET", routePath(rbac.VendorWrite), "Bearer "+mixed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer+buyer status = %d, want 200", resp.StatusCode)
	}
}
