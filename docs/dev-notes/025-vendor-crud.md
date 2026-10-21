# 025 — Vendor CRUD API

- **Issue:** #25
- **Branch:** feature/25-vendor-crud
- **Date:** 2026-10-21

## What
- **Procurement service bootstrap** (first domain in the service): migrations on boot, health (liveness/readiness), gRPC API, `cmd/migrate` for CI.
- **`procurement.vendors`** (migration 0001): tenant-scoped, `UNIQUE (tenant_id, code)` (codes unique per tenant only), soft-delete via `status`, audit columns (`created_by`/`updated_by` from the propagated user). Same RLS pattern as identity (FORCE + `app.tenant_id` policy + `eproc_app` grants).
- **`vendor` domain**: validation (code `^[A-Z0-9][A-Z0-9-]{1,31}$` after uppercasing, name 2–200, email format), machine error codes (`vendor_duplicate_code`, `vendor_not_found`, `vendor_deactivated`, …), store where every query runs through `tenant.WithTenant` — no tenant-spanning path exists. **`GetForUse` is the active-vendor gate PO creation (#35) must call.**
- **gRPC `procurement.v1`**: Create/Get/Update/DeactivateVendor; the service-layer RBAC map is populated for the first time (`vendor:write` / `vendor:read`) — defense in depth in action.
- **Gateway REST** `/api/v1/vendors` (POST/GET/PATCH/:id/deactivate): Bearer + `RequirePermission`, claims propagated to procurement via `rbac.OutgoingContext` (the #23 hook's first real use), gRPC→HTTP error mapping with machine codes preserved (`{"error": human, "code": machine}`).
- **Refactor**: identity's server internals (health, trace/log/recovery interceptors, readiness loop) extracted to `internal/platform/grpcserver`; identity is now a thin adapter. Procurement consumes it as-is.
- Infra: procurement in compose (generic Dockerfile, gRPC healthcheck, port 8082, gateway depends on it), `PROCUREMENT_ADDR`/`PROCUREMENT_PORT` env, CI runs `cmd/migrate` for both services. `docs/schema/procurement.md`.

## Decisions
- Codes unique per tenant, not globally — matches the schema decision from #19 and the demo data; two tenants may share a code.
- Deactivated vendors: immutable + `GetForUse` fails — "cannot be used in new POs" is enforced at the domain gate that PO creation will call (#35), not by deleting data (soft-delete only, rows stay readable for history).
- Update uses empty-means-keep instead of field masks — smallest contract that works for the UI; masks can come with #27's list/filter work if needed.
- gRPC messages carry machine codes verbatim; the gateway adds human sentences — one mapping table, no code parsing on either side.

## Evidence (all green; integration against real PG with RLS)
- AC *duplicate code rejected*: store test (same tenant → `ErrDuplicateCode`; same code other tenant → OK) + gRPC lifecycle (AlreadyExists + `vendor_duplicate_code`) + gateway (409 with machine code in body).
- AC *deactivated vendors unusable in new POs*: `GetForUse` fails after deactivate, deactivated is immutable, row stays readable, second deactivate reports state; gRPC 412-path (FailedPrecondition).
- AC *audit fields*: create/update stamp and return `created_at/by`, `updated_at/by`; test asserts updated_by changes while created_by survives.
- Cross-tenant: get/update/deactivate from tenant B → NotFound (indistinguishable from missing).
- RBAC: viewer → 403 at gateway and PermissionDenied at the service layer (in-process gRPC with propagated roles); missing metadata → Unauthenticated.
- Live compose smoke (below) exercises the full path REST → gateway → metadata → procurement → RLS → PG.
- Gates: `go vet/build` clean, `go test ./...` green (16 packages), pnpm gates pass, `docker compose config` valid; compose `up -d --wait --build` → 8 services healthy.

## Follow-ups
- #26 attachments; #27 list/search/pagination; #35 PO creation must route vendor selection through `GetForUse`.
