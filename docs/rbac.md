# RBAC model — roles, permissions, and enforcement

Single source of truth for the role/permission matrix: [`internal/platform/rbac`](../../internal/platform/rbac/rbac.go). The Go package and this document must stay in sync — the package's test suite (`TestMatrixCoversEveryPermission`, `TestEveryRoleAgainstMatrix`) enforces the code side.

## Roles

Fixed catalog (seeded in `identity.roles`): **admin, requester, approver, buyer, finance, viewer**. A user may hold several roles; any one of them granting the permission is enough.

## Permission matrix

`admin` implicitly holds every permission; the table lists the other roles per permission.

| Permission | requester | approver | buyer | finance | viewer |
|---|---|---|---|---|---|
| `requisition:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `requisition:write` | ✓ | | | | |
| `approval:decide` | | ✓ | | | |
| `vendor:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `vendor:write` | ✓ | | | | |
| `po:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `po:write` | | | ✓ | | |
| `grn:post` | | | ✓ | | |
| `invoice:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `invoice:write` | | | | ✓ | |
| `export:run` | | | | ✓ | |
| `audit:read` | | | | | ✓ |
| `users:manage` | | | | | |

Read permissions are broad by design (everyone in the tenant sees the flow they participate in); writes are narrow. Ownership rules are domain checks on top of RBAC, enforced inside the owning service. Landed so far: **requisition edit/submit (#29, maintainer decision 2026-10-23)** — the caller must be the requisition's `requester_id`; admins may act on any requisition of the tenant; violations return `403` with machine code `requisition_forbidden`. Approver-scope rules ("an approver decides *their assigned* step") arrive with the approval service (#30/#31).

## Enforcement layers (defense in depth)

1. **Gateway middleware** (`services/gateway/server`): after Bearer-token verification, `RequirePermission("<permission>")` checks the token's roles. Failure → `403` with a machine-readable body:

   ```json
   {"error": "forbidden", "code": "permission_denied", "required": "vendor:write"}
   ```

2. **Service-layer gRPC interceptor** (`internal/platform/rbac.UnaryInterceptor`): each service maps its user-facing methods to required permissions (`MethodPermissions`); the interceptor re-checks the roles the gateway propagated in gRPC metadata (`x-role`, with `x-user-id` / `x-tenant-id` alongside). Methods absent from the map are internal or pre-auth. Failures:
   - protected method called without caller metadata → `Unauthenticated` (`auth context missing`)
   - roles insufficient → `PermissionDenied` (`permission_denied: requires <permission>`)

3. **PostgreSQL RLS** — tenant tables run under `FORCE ROW LEVEL SECURITY`; policies key on the transaction-local `app.tenant_id` set by `internal/platform/tenant.WithTenant` (#23). The gateway injects the token's `tid` into gRPC metadata (`rbac.OutgoingContext`), services read it back (`rbac.FromMetadata`), and every tenant-scoped query runs through `WithTenant` as the non-superuser `eproc_app` role.

The gateway never sees credentials and the services never see the JWT — only the verified claims, so neither layer can be bypassed by the other trusting too much.

## Adding or changing a permission

1. Add/change the entry in the `matrix` slice in `internal/platform/rbac/rbac.go` and in `AllPermissions` (constants).
2. Mirror the change in the table above and in `rbac_test.go`'s `wantPermission`.
3. Register the middleware on the gateway route and the method in the owning service's `MethodPermissions`.

`TestMatrixCoversEveryPermission` fails if the code matrix and the test's documented expectations diverge.
