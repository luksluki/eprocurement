# Dev note — #27 Vendor list search, filter, and pagination

## What changed

- `proto/procurement/v1` + regen: `ListVendors` (query/status/page/page_size
  → vendors + total/page/page_size).
- Migration `0002_vendor_list_indexes.sql`: `pg_trgm` extension + GIN
  trigram index on `name`; `(tenant_id, status, code)` replaces 0001's
  `(tenant_id, status)`.
- `vendor.ListFilter` + `ValidateList`: status whitelist, page/page_size
  clamping (defaults 1/20, max size 100), query trimmed + uppercased
  (codes are canonical uppercase) with LIKE wildcards escaped so user
  input matches literally.
- `Store.List`: one query with `count(*) OVER()` (window total rides the
  page); a page past the end emits no window rows, so an explicit COUNT
  fallback recovers the true total. Tenant scoping is RLS as everywhere
  else.
- gRPC `ListVendors` (`vendor:read` in the method map) + REST
  `GET /api/v1/vendors?q=&status=&page=&page_size=` on the gateway;
  `vendor_status_invalid` / `vendor_page_invalid` map to 400.

## Evidence

- `go vet ./... && go build ./...` clean; full `go test ./...` (incl.
  integration against local PostgreSQL) green; `pnpm build/lint/test` green.
- `TestStoreList*` (store): filters combine correctly (status × code
  prefix × name substring, both orderings), literal `%` in queries,
  pagination incl. past-the-end totals, cross-tenant invisibility.
- `TestVendorGRPCList` (full wiring): defaults, combined filters,
  cross-tenant, InvalidArgument for bad status/page, Unauthenticated
  without metadata. `TestVendorListRoute` (gateway): query params
  forwarded, JSON envelope, tenant propagation, 400 mapping.
- `TestStoreListScale100k` (AC: list works with 100k rows under budget):
  100,000 vendors in one tenant, ANALYZEd, every shape ×20 under RLS.
  p95 per query: 16–78 ms against the 300 ms budget. EXPLAIN (ANALYZE)
  plans captured in the test log and mirrored in
  `docs/schema/procurement.md`.

## Decisions / findings

- **Budget reference**: the issue says "under budget" without a number;
  300 ms p95 was used, matching the PRD's performance bar (FR-09).
- **Why the plans show sequential scans (documented, not a bug):**
  1. `total` uses `count(*) OVER()`, so all matching rows are visited
     regardless of LIMIT; at 100k rows the ~1,500-page heap is cheaper
     than index+heap lookups. Same plan shape without RLS.
  2. The #23 RLS policy's bypass arm (`tenant_id = … OR rls_bypass = 'on'`)
     can never be planned as an index qual, so tenant scoping always runs
     as a filter. Verified experimentally that a uuid-cast policy variant
     does not change this (the OR arm is the blocker) — fixing it means
     reshaping the shared platform policy, out of scope for this issue
     and recorded as a follow-up consideration.
- COPY seeding was rejected by PostgreSQL (`COPY FROM not supported with
  row-level security`) — the scale fixture uses batched multi-row INSERTs
  under the bypass context instead.

## Follow-ups

- If list latency ever matters beyond the budget: policy reshaping or a
  per-service read role to unlock tenant-leading index quals (platform
  decision, affects #23's design).
- `total` makes every list visit all matches; if that ever dominates,
  switch to a cheaper "has next page" or cached counts for the UI.
