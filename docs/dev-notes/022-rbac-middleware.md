# 022 — RBAC model and middleware

- **Issue:** #22
- **Branch:** feature/22-rbac (stacked on feature/21-jwks-rotation — PR #79 was still open when this started)
- **Date:** 2026-10-21

## What
- `internal/platform/rbac`: one declarative permission matrix (13 permissions × 6 roles, admin implicit) + `Check`, shared by both enforcement layers so the edge and the services cannot drift.
- Gateway middleware `RequirePermission` (runs after `bearerAuth`): 403 with a machine-readable body — `{"error":"forbidden","code":"permission_denied","required":"<permission>"}`.
- Service-layer `rbac.UnaryInterceptor(MethodPermissions)`: re-checks permissions from the gRPC metadata the gateway propagates (`x-role` repeated, `x-user-id`, `x-tenant-id`); mapped methods missing caller metadata → `Unauthenticated`, insufficient roles → `PermissionDenied` with `permission_denied: requires <permission>`. Unmapped methods pass (internal/pre-auth).
- `rbac.FromMetadata` extracts the `AuthContext` (user/tenant/roles) for services to scope queries — the hook #23 builds on.
- Identity server installs the interceptor in its chain (empty map today: Login/Refresh/Logout/GetJwks are pre-auth/internal; ListUsers/AssignRoles register theirs when they land).
- `docs/rbac.md`: the documented matrix + reason codes + enforcement layers + change procedure; linked from README.

## Decisions
- Matrix as code (single source of truth) with `docs/rbac.md` mirroring it; `TestMatrixCoversEveryPermission` + `TestEveryRoleAgainstMatrix` fail if code and documented expectations diverge.
- Read permissions broad (all six roles), writes narrow, ownership rules (own-requisition, assigned-step) deliberately left to the domain layer — they need entity context RBAC does not have.
- Missing-auth on a protected method is `Unauthenticated`, insufficient roles is `PermissionDenied` — the gateway maps both to 401/403 respectively.
- No fake production routes: the route matrix is exercised through real `bearerAuth`+`RequirePermission` on test-registered routes.

## Evidence
- `TestEveryRoleAgainstMatrix`: all 6 roles × all 13 permissions against the matrix; `TestCheckSemantics` (empty/blank/unknown roles never pass, any-granting-role semantics, unknown permission).
- `TestInterceptorEnforcesMatrix`: every role through the service-layer interceptor on a protected method (OK vs PermissionDenied); `TestInterceptorDenials` (no metadata, no roles, blank roles, reason message with required permission); unmapped methods pass without metadata.
- `TestRouteMatrixAllRolesAllPermissions`: every role × permission through the real fiber stack with minted tokens — asserts 200 or 403 **and** the 403 body reason code; `TestRequirePermissionMultiRoleToken` (viewer denied, viewer+buyer allowed).
- Gates: `go vet/build` clean, `go test ./...` green (incl. integration), pnpm gates pass, `docker compose config` valid; identity+gateway images rebuilt, stack healthy (`/healthz` ok, gRPC probe SERVING).
- Two test-authoring pitfalls found and fixed along the way: fiber treats `:` in a path as parameter syntax (`/test/vendor:write` and `/test/vendor:read` collapse into one route — permissions now map to `/test/<area>/<action>`), and `bearerAuth(nil)` panics on first token (tests now use the real verifier).

## Follow-ups
- #23: gateway injects the claims into gRPC metadata (the interceptor's read side is ready) + tenant RLS.
- Gateway REST auth endpoints (login/refresh/logout) will carry `RequirePermission` on their business routes.
