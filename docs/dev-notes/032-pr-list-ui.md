# 032 — PR list UI with filters and server-side pagination

- **Issue:** #32
- **Branch:** feature/32-pr-list-ui
- **Date:** 2026-10-24

## What

- **`ListRequisitions` end-to-end, following the #27 vendor-list pattern** —
  the list endpoint did not exist (the proto had only `ListVendors`;
  docs/schema/procurement.md:352 had deferred read endpoints), so this issue
  ships it: proto (`ListRequisitionsRequest/Response`, header-only rows
  documented in the proto comments) + `make gen-proto`; `requisition.ListFilter`
  + `ValidateList` (six-status whitelist, page/page_size clamping 1/20/max
  100, query trimmed + uppercased with escaped LIKE wildcards);
  `Store.List`; gRPC handler (`requisition:read` in the method map — every
  role holds it per docs/rbac.md); gateway
  `GET /api/v1/requisitions?q=&status=&page=&page_size=` with the
  `{requisitions, total, page, page_size}` envelope; new 400 codes
  `requisition_status_invalid` / `requisition_page_invalid` carried through
  the shared gRPC→HTTP mapping.
- **`Store.List` differs from #27's `count(*) OVER()` on measured evidence**
  (see Decisions): one statement, page subquery (`ORDER BY number COLLATE "C"
  DESC LIMIT/OFFSET`) + exact COUNT as an uncorrelated scalar subquery
  (executor InitPlan, evaluated exactly once) + the same past-the-end COUNT
  fallback as the vendor list. Rows are **header-only** — items and approval
  steps are never loaded. Ordering is `number DESC` (newest first: numbers
  are sequential per tenant).
- **Scale proof** — `TestStoreListScale100k` (requisition package) mirrors
  #27's: 100k requisitions in one tenant via batched multi-row INSERTs under
  the bypass context, `VACUUM (ANALYZE)` + `CHECKPOINT` to reach the
  production steady state, then 4 shapes × 30 timed runs under the real RLS
  context at p95 < 300 ms; EXPLAIN (ANALYZE, BUFFERS) plans logged and
  mirrored in docs/schema/procurement.md.
- **Web `/app/requisitions`** — the PRD §11 list screen. Filters
  (`q`, `status`, `page`, `page_size`) live in the URL (single source of
  truth): reload and back/forward restore the same view, every filter change
  navigates with page reset to 1, and `filtersToSearchParams` omits defaults
  so one view always maps to one URL. **TanStack Query** fetches through a
  thin BFF route `GET /api/requisitions` (query key = the filter tuple;
  `placeholderData: keepPreviousData` for page transitions); **TanStack
  Table** renders with `manualPagination: true` — the client holds exactly
  one page (default 20, max 100) and never all rows. Distinct empty ("no
  requisitions yet", with the onboarding pointer) vs no-results ("no match",
  with a reset-filters action), loading, error-with-retry, and 401 → refresh
  dance states. `Providers.tsx` (QueryClientProvider) added to the shell
  layout.
- **Docs** — schema section "Requisition list — filters, pagination, query
  plans (#32)" with the budget table and plan summaries; onboarding §5.1
  step 2b (seed requisitions + approval rules so the screen has data).

## Decisions

- **Exact COUNT as a scalar subquery (InitPlan), not `count(*) OVER()`.**
  The AC named the #27 window pattern; at 100k rows on default `work_mem`
  (4 MB) the window tuplestore (~12 MB) spilled to temp files — measured
  217–540 ms per unfiltered query against the 300 ms budget, on both this
  machine's plans (`temp read=1913 written=1119`). An intermediate
  `CROSS JOIN (SELECT count(*) …)` form was also rejected with evidence:
  without a Materialize node the planner re-executed the aggregate per page
  row (`loops=20`), turning a 50 ms query into ~1 s. The chosen form is
  evaluated exactly once by executor construction. The single-statement /
  one-roundtrip property and the past-the-end total contract are unchanged;
  flagged here for PR review since it intentionally deviates from the
  plan's phrasing.
- **`ORDER BY number COLLATE "C" DESC`.** Every storable requisition number
  is canonical server-generated ASCII (`PR-%06d`), so C-collation ordering
  is identical to the default collation for this data; on the dev machine's
  libc collation (English_Indonesia.1252) the top-N sort alone dropped from
  ~140 ms to ~35 ms (Windows locale comparisons are function calls; "C" is
  memcmp). Not applied to the vendor list — that is #27's scope.
- **Fixture hygiene is part of the budget test**: `VACUUM (ANALYZE)` after
  the bulk load (what autovacuum does in production) sets the visibility map
  — without it the count arm's index-only scan did 100k heap fetches — and
  an explicit `CHECKPOINT` keeps the fixture's dirty buffers from competing
  with the timing loop (seen as 100 ms–2 s outliers). 30 measured runs after
  3 warm-ups; p95 index tolerates one OS outlier. Documented because #27's
  harness (ANALYZE only, 20 runs) was not reproducible on this machine.
- **BFF route returns a discriminated result**: gateway 401 → 401 (the
  client navigates to `/api/auth/refresh?next=<path+query>`, which is
  `safeNext`-legal and returns to the same filtered view); any other
  failure → 502 `gateway_unavailable` for the retry state. Inputs are
  canonicalized through `requisition-filters` before forwarding, so gateway
  4xx never happens in practice.
- **`q` matches a number prefix only** (no name/description field exists on
  the requisition header; items are deliberately not searched — header-only
  list, per-row item scans would break the budget contract).
- Dev-note filename follows the zero-padded convention (`032-…`), matching
  the expected-files list.

## Evidence

- `go vet ./services/procurement/... ./services/gateway/...` — clean; full
  `go test ./services/procurement/... ./services/gateway/... -count=1` green
  against local PostgreSQL 17 (`TEST_DATABASE_URL` to a scratch
  `eproc_test_32` database; Docker Desktop unavailable this session, the
  compose stack was not running).
- `TestStoreListScale100k` passed 3 consecutive runs; worst shape p95 251 →
  258 → 180 ms (budget 300). Medians: unfiltered 64–88 ms, status 8–10 ms,
  prefix 56–156 ms, combined 74–161 ms. EXPLAIN plans captured in the test
  log and summarized in docs/schema/procurement.md.
- Store tests (`TestStoreListFilters`, `TestStoreListPagination`,
  `TestValidateListErrors`): filters combine (status × prefix × literal `%`,
  both status orderings), header-only rows, descending order, past-the-end
  total, defaults, and a cross-tenant denial (tenant B's listing sees only
  its own row; tenant A's never leak).
- gRPC (`TestRequisitionGRPCList` through the real server + RBAC map):
  defaults, filters, header-only, newest-first, cross-tenant invisibility,
  InvalidArgument for bad status/page, Unauthenticated without metadata.
  Gateway (`TestRequisitionListRoute`): forwarded params, envelope, viewer
  allowed (requisition:read is broad), 400 machine codes.
- Web: `pnpm vitest run` — 46/46 across 8 files (17 new: filter parse/
  serialize round-trips incl. clamping and unknown-status drops; BFF route —
  no-cookie 401 without a gateway call, canonical forwarding with the bearer
  token, garbage-input clamping, gateway 401 passthrough, 500/network →
  502). `pnpm lint` clean; `pnpm build` green (`/app/requisitions` 18.2 kB
  client bundle, `/api/requisitions` handler present, all §11 routes still
  rendered).

## Follow-ups

- **PR review should confirm the `Store.List` count-shape deviation** from
  the plan's `count(*) OVER()` wording (measured rationale above); if
  preferred, #27's vendor list could adopt the same InitPlan form for
  consistency (its budget passes either way).
- The RLS policy's per-row `tenant_id::text = current_setting(…)` evaluation
  is now the dominant cost of full-scan shapes at 100k rows; the
  platform-level policy reshaping (uuid-compare variant + dropping the OR
  arm for a dedicated read role) remains the #27 follow-up — more valuable
  with every additional list endpoint.
- Live end-to-end (gateway + web `pnpm dev` + browser click-through of the
  filter/pagination/empty/error states) was not run this session — Docker
  was unavailable; CI plus the unit/integration suites cover the wiring. A
  manual pass per onboarding §5.1 step 2b is the natural PR-demo step.
- The 2s-scale OS stalls observed on this Windows box during sustained
  loops (the reason for 30 runs + checkpoint hygiene) do not appear on the
  Linux CI runners; if CI ever shows the same tail, the harness numbers to
  revisit are in the schema doc's budget table.
