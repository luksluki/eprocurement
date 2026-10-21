# 023 — Tenant context propagation and PostgreSQL RLS

- **Issue:** #23
- **Branch:** feature/23-tenant-rls (stacked on feature/22-rbac; PRs #79/#80 still open)
- **Date:** 2026-10-21

## What
- **RLS migrations (identity 0002 + 0003):** `ENABLE/FORCE ROW LEVEL SECURITY` on `users`, `user_roles`, `refresh_tokens`; policies compare `tenant_id` to the transaction-local `app.tenant_id` (`WITH CHECK` blocks cross-tenant writes; child rows visible iff their parent user is). 0003 creates the non-superuser role `eproc_app` with scoped grants — superusers bypass RLS even with FORCE, and the local/CI bootstrap user (`POSTGRES_USER`) is one.
- **`internal/platform/tenant`:** `WithTenant(tenantID)` = tx + `SET LOCAL app.tenant_id` + `SET LOCAL ROLE eproc_app` (the only way app code should reach tenant data); `WithBypass` = tx + `SET LOCAL ROLE eproc_app` + `app.rls_bypass=on` for the auth flows that legitimately span tenants (login email lookup, refresh, logout).
- **Auth flows moved under the helpers:** `loadUser`, `Refresh` (rewritten as `refreshTx` inside `WithBypass`, committing the family-revocation before returning the generic invalid-token error), `Logout`, and `issuePair`. Test fixtures and `useradmin` likewise.
- **gRPC metadata propagation (gateway side):** `rbac.OutgoingContext(ctx, userID, tenantID, roles)` attaches the verified claims as outgoing metadata; the service side (interceptor + `FromMetadata`, from #22) reads it back. Round-trip unit-tested. The JWT `tid` claim has existed since #20.
- Docs: schema/identity.md (RLS mechanics), rbac.md (layer 3 now real).

## Decisions
- **App role switch instead of app reconnect:** the compose/CI bootstrap user is a superuser and would silently bypass every policy; rather than change connection strings everywhere, each tenant-helper transaction drops to the non-superuser `eproc_app`. Migrations keep superuser (DDL needs it).
- **Scoped bypass flag for the auth flows:** login must search emails across tenants (exactly-one-match rule) and refresh/logout address tokens without a tenant context. `app.rls_bypass` is transaction-local, set only through `WithBypass`, and documented — not a client-controllable value.
- **No business gRPC call exists yet** to carry the metadata end-to-end (procurement arrives in M2); propagation is complete at both ends and round-trip tested. The first business proxy will exercise it live.

## Evidence (all integration, `TEST_DATABASE_URL`, RLS active)
- AC *cross-tenant reads/writes fail:* tenant A sees exactly its 1 user, tenant B its 1; inserting tenant B's row under tenant A's context → `row-level security` violation.
- AC *RLS blocks bypass attempts:* as `eproc_app` with no `app.tenant_id`, `SELECT count(*)` sees 0 rows of a populated table.
- AC *tenant switching works:* the same caller scoped A → B → A reads each tenant's row and only that one.
- Auth under RLS: login → refresh → logout all pass; the full identity suite (rotation, family revocation, enumeration, gRPC e2e) green.
- Found during verification: the first run "passed" writes it should have rejected — the bootstrap superuser bypassed RLS entirely, leaving a polluting row; that is what surfaced the app-role requirement (and the tests now clean up after a hypothetical regression).
- Gates: `go vet/build` clean, `go test ./...` green (incl. integration), pnpm gates pass; compose identity rebuilt and healthy after applying 0002+0003 on boot.

## Follow-ups
- Production posture: connect services as a non-superuser directly (then `SET LOCAL ROLE` becomes redundant defense); needs a compose/infra pass.
- First business RPC (vendor CRUD, #25) exercises the metadata propagation live; procurement tables reuse the same tenant policies.
- Web-shell tenant switcher (#50) re-issues tokens per tenant — platform-side switching is proven here.
