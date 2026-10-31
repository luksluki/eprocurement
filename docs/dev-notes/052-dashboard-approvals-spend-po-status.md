# Dev note — #52 Dashboard: approvals, spend, PO status

- **Issue:** #52
- **Branch:** feature/52-dashboard-approvals-spend-po-status
- **Date:** 2026-10-31

> **Provenance:** the maintainer answered the open questions on issue #52
> (comments, 2026-10), so no agent-taken decision needed the 035/036/037
> protocol beyond the two recorded below:
>
> 1. **Seed follows now (option (a), as amended).** `scripts/seed` anchors
>    its base at `now − 120 days` and — the amendment — distributes the N
>    requisitions **evenly** across the 120 days (per-index spacing =
>    120d/N ≈ 1.73 min at the 100k default), because the literal base-only
>    arithmetic left the 30-day window empty (the default 1-minute spacing
>    spans only ~69 days). The relative offsets (+2h issue / +24h GRN / +48h
>    invoice) shift along; RNG and document ordering stay deterministic.
>    Touching `scripts/seed` was approved for exactly this change.
> 2. **Demo loading is out of scope for #52.** Evidence for p95 and
>    reconciliation uses scale fixtures in the `procurement.*` schema (the
>    dev-note #41 protocol approved at #41); synchronizing the `demo.*`
>    dataset into `procurement.*` is a separate issue. The formal invoice
>    budget remains with #56.

## What changed

- **Proto + gen** — additive `GetDashboard` RPC with
  `GetDashboardRequest`/`DashboardSummary` and the widget messages
  (`PendingApprovals`/`PendingApproval`, `VendorSpend`,
  `PurchaseOrderStatusCount`, `CurrencySpend`). `make gen-proto`
  regenerated only the procurement gen files (the documents gen churn was
  line-endings only and reverted, the #41 protocol).
- **`services/procurement/dashboard`** — the FR-09 read model. `Store.Summary`
  runs five queries in ONE `tenant.WithTenant` transaction: the pending
  count (EXISTS guard) and first page (DISTINCT ON the smallest pending
  `step_no`, oldest-submitted first, bounded to 5), the per-vendor+currency
  spend over the 30/90-day windows (one statement, FILTER arms) plus the
  per-currency KPI totals, and the status distribution zero-filled to all
  seven FR-06 statuses. Query texts are package constants so the scale test
  EXPLAINs the exact statements. No new tables; pure read side.
- **Migration `0014_dashboard_read_indexes.sql`** — three partial read
  indexes: `requisitions_dashboard_pending_idx (tenant_id, submitted_at)
  WHERE status='submitted'`, `approval_steps_pending_idx
  (requisition_id, step_no) WHERE status='pending'`, and
  `purchase_orders_dashboard_spend_idx (tenant_id, issued_at) WHERE
  issued_at IS NOT NULL AND status NOT IN ('draft','cancelled')`.
- **rbac** — the new broad `dashboard:read` (requester/approver/buyer/
  finance/viewer; admin implicit) in the matrix, `rbac_test.go`'s
  `wantPermission`, docs/rbac.md's table + landed entry, the gateway route,
  and procurement's `methodPermissions`.
- **procurement server** — `GetDashboard` handler (`authTenant` + one store
  call; IO failures map to Internal like every read mapper's default arm),
  `dashboardPB`, and the `dashboard.Store` threaded through `server.New`
  and `cmd/procurement`.
- **gateway** — `ProcurementClient` widened with `GetDashboard` (the base
  fake in `requisitions_test.go` satisfies it for every other test);
  `GET /api/v1/dashboard/summary` behind Bearer + `RequirePermission(
  rbac.DashboardRead)` rendering the envelope `{pending_approvals,
  spend_by_vendor, po_status_distribution, spend_totals}`.
- **web `/app/dashboard`** — the placeholder becomes live: four KPI cards
  (pending queue, 30d/90d spend per currency, PO total), the approvals
  table (each row linking to its requisition detail, step + approver role),
  the spend table (money formatted per currency via `Intl.NumberFormat`),
  and the status distribution with badges; `loading.tsx` (route-level
  skeleton, dictionary-aware), `ErrorState` on fetch failure, `EmptyState`
  inside each widget; copy ID+EN through the dictionary.
- **`apps/web/src/lib/gateway.ts`** — `getDashboardSummary` (the
  listVendors data-or-null convention) + the wire types.
- **`scripts/seed`** — the now−120d base with the even 120-day spread
  (decision 1); the header comment documents the anchoring.
- **Docs** — `docs/schema/procurement.md` (the "Dashboard summary (#52)"
  section: contract, reconciliation, 0014 indexes, measured plans, seed
  anchoring; follow-ups entry), `docs/rbac.md` (matrix row + landed entry).

## Decisions

- **The migration lands as 0014, not 0013.** The plan's expected filename
  `0013_dashboard_read_indexes.sql` collides with #44's already-landed
  `0013_outbox_actor_ip.sql`; the next free slot is 0014 (verified:
  `schema_migrations` tops out at 14 after applying on a scratch database).
- **Spend basis = the #49 register basis**: POs with `issued_at` inside the
  window and status neither draft nor cancelled (drafts never carry
  `issued_at`; cancelled POs that were issued drop out). Pinned by the AC
  citing #49's PO register.
- **KPI spend totals are per currency, and the vendor table is bounded to
  the top 10 rows by 90-day spend.** Money never mixes IDR and USD, so a
  scalar total would be wrong for a mixed tenant; the unbounded per-currency
  sums live beside the bounded table (the scale fixture's 24 in-window
  vendor+currency pairs overflow the bound, so the KPI/table split is
  observable, not decorative). Ordering ties break on `vendor_id` for
  deterministic pagination.
- **Pending count uses an EXISTS guard, not bare `status='submitted'`** — a
  submitted requisition with no pending steps (impossible via the domain:
  decide moves the status) must count for nothing; the scale fixture plants
  such rows to prove it.
- **`approval_steps_pending_idx` was added after measuring.** The first
  budget runs passed without it but the pending page seq-scanned +
  sorted `approval_steps` (~220 ms of the ~300 ms call at 160k steps); the
  partial index (pending rows only, already in DISTINCT ON order) halved
  that arm. Pending steps are transient in production, so the index stays
  small on every write path it touches.
- **`dashboardMessages` is empty on purpose**: the read has no machine codes
  of its own (the request is empty — anonymous/under-privileged calls are
  answered by middleware before the handler), so only the shared
  Internal → 502 shape can surface. The handler is still wired through
  `grpcHTTPError` for that arm.
- **The dashboard page fetches once, as a server component** (the vendors
  list pattern) — no client fetcher, no BFF proxy: the summary is a pure
  read with no action routes.
- **Seed determinism note:** `seedBase()` truncates to the hour so
  consecutive runs within an hour produce identical relative timestamps;
  across hours the anchor moves with the clock (that is the decision's
  intent — the data follows now).
- **Scale-fixture cleanup retries once.** Right after a 300k-row bulk load
  the compose PostgreSQL (16.15) intermittently answered the big DELETE
  with `MultiXactId … has not been created yet -- apparent wraparound`
  (XX000, transient — a fresh transaction seconds later succeeds). A silent
  cleanup failure left the fixture behind and broke the next run on
  duplicate keys, so the retry (and a hard failure if it repeats) is
  load-bearing.

## Evidence

Against the local compose stack
(`TEST_DATABASE_URL=postgres://eprocurement:eprocurement@localhost:55432/eprocurement?sslmode=disable`),
packages run sequentially `-p 1` (the dev-note 031 protocol):

- `go build ./...` and `go vet ./...` — clean, including all test files.
- `go test ./internal/platform/rbac/ -count=1` — green (the matrix tests
  cover the new `dashboard:read` row through `TestMatrixCoversEveryPermission`
  and `TestEveryRoleAgainstMatrix`).
- `go test ./services/procurement/dashboard/ -count=1` — green:
  `TestStoreSummaryReconciles` (the reconciliation AC on a domain-driven
  small fixture: 7 submitted → total 7 with the first page of 5 oldest-first
  and each row the current step; spend equals the client-side per-vendor+
  currency sums over the paged PO list with the window filter applied row by
  row; distribution zero-fills `partially_received` and sums to the
  unfiltered list total), `TestStoreSummaryCrossTenantDenial` (tenant B sees
  an empty snapshot), `TestStoreSummaryFailurePath` (empty tenant id →
  error), `TestStatusOrderCanonical`, and `TestStoreSummaryScale100k`
  (below).
- **`TestStoreSummaryScale100k`** — the p95 + reconciliation + EXPLAIN AC on
  ~100k rows in `procurement.*` (100k requisitions, ~160k steps, 40k POs,
  16 vendors, IDR+USD, issued_at spread inside/outside both windows,
  `VACUUM (ANALYZE)` + `CHECKPOINT` fixture hygiene):
  - *Reconciliation subtest passes*: pending total matches the tracked queue
    (19,975 — the fixture's step-less submitted rows excluded); the
    distribution sums to the unfiltered `ListPurchaseOrders` total (40,000);
    the 10 spend rows match the tracked per-vendor+currency sums in
    descending 90-day order; the per-currency totals match the unbounded
    sums and provably exceed the bounded rows' sums (24 in-window pairs vs
    the 10-row bound).
  - *p95*: warm-ups + 20 timed iterations, attempt 1/3: **median 280.7 ms,
    p95 340.7 ms < the 500 ms budget** (PRD §19's dashboard row). Across the
    earlier debug runs the whole-call p95 ranged 264–456 ms — the spread
    tracked database churn from immediately preceding bulk loads; the
    shapeAttempts=3 retry protocol (the #38/#41 contention guard) bounds
    that in CI.
  - *EXPLAIN (ANALYZE, BUFFERS)* for all five queries logged by the test and
    mirrored in `docs/schema/procurement.md` — pending count ~52 ms, pending
    first page ~101 ms (on `approval_steps_pending_idx`), spend totals
    ~11 ms, spend by vendor ~133 ms (both on
    `purchase_orders_dashboard_spend_idx`), distribution ~17 ms.
- `go test ./services/procurement/server/ -count=1 -run TestDashboardGRPC` —
  green (the summary through the full gRPC wiring: viewer passes the broad
  read, the envelope matches a real submit→decide→convert→issue flow,
  cross-tenant emptiness, anonymous Unauthenticated, unknown-role
  PermissionDenied).
- `go test ./services/gateway/server/ -count=1 -run TestDashboard` — green
  (route through `New` + `WithProcurement` with the full envelope + claims
  as gRPC metadata; 401 anonymous before any procurement call; 403 for a
  role holding nothing; Internal → 502 through the shared mapping).
- Migration 0014 on a **fresh scratch database** (`eproc_dash_52`, dropped
  afterwards): `go run ./services/procurement/cmd/migrate` applies 14
  migrations; all three 0014 indexes exist; `schema_migrations` tops out at
  14.
- **Seed gates:** `make seed` — 44.3 s (< the 2-minute budget), and the
  windows fill as the decision promised: 30d holds 16,737 of 66,787 POs
  (~25%), 90d holds 50,227 (~75%), oldest `issued_at` = 120 days back,
  newest = today. `make seed-reset` — `demo.*` tables at 0 rows and the
  Mongo `events`/`audit_log` collections at 0 documents.
- **Manual list-vs-summary reconciliation** (the AC's manual leg, run by
  hand through psql outside the test harness — the store's queries verbatim
  under `SET ROLE eproc_app` + `app.tenant_id`, against a 9-requisition /
  6-PO / 2-vendor scratch fixture covering every window/status/currency
  arm, deleted afterwards):
  - summary pending count **3** with the first page `PR-520001/2/3` each at
    step 1/approver oldest-first;
  - summary spend `PT Manual A` IDR (30d **1,000,000** / 90d **3,000,000**)
    and USD (500/500) — byte-identical to the independently computed
    per-vendor+currency sums over the register basis; vendor B absent on
    both sides (its only POs are out-of-window, cancelled, and draft);
  - distribution answered 6 statuses × 1 (partially_received zero-filled)
    summing **6** == the unfiltered `ListPurchaseOrders` count **6**.
- Web (apps/web): `npx vitest run` — **356/356 green** (48 files; includes
  the i18n parity test over the rewritten `dashboard` dictionary sections).
  `npx tsc --noEmit` — 0 errors outside test files; the errors inside
  pre-existing test files are unchanged from pristine HEAD (verified count
  on the clean tree per the #41 protocol). `npx next build` — green,
  `/app/dashboard` in the route table (179 B client); `npx eslint .` —
  clean.
- Not run by me (the workflow script re-runs the full gate set per the
  ask): the repo-wide `go test ./...`, the root `pnpm install
  --frozen-lockfile && pnpm build && pnpm lint && pnpm test`, and
  `docker compose config/up/ps` (no compose file changes).

## CI round 1 — root cause: unindexed FK checks, fix commit

The first CI round failed on the Go job (run 41198373932, `gh run view
--log-failed`): **all four ~100k scale packages hit `go test`'s default
10-minute package timeout** — `FAIL …/procurement/dashboard 600.015s`,
`…/purchaseorder 600.064s`, `…/requisition 600.073s`, `…/vendor 600.011s`,
each `panic: test timed out after 10m0s`. This issue's dashboard fixture
made it four concurrent scale fixtures against CI's one Postgres (previously
three), but the load alone was not the killer — the dumps show packages
stuck in their cleanup DELETEs.

**Root cause (measured, not inferred): the NO-ACTION FK triggers on
`purchase_orders.requisition_id` and `purchase_orders.vendor_id` seq-scanned
`purchase_orders` per deleted parent row** — no index leads with the
referencing column (the 0007 `UNIQUE (tenant_id, requisition_id)` leads with
`tenant_id`, unusable for the FK check). Reproduced by hand through psql:
`DELETE` of 100k requisitions referenced by 50k POs took **727,850 ms
(12 min)**; the same DELETE with the new indexes is sub-second. So whenever
another fixture's POs were present (always, in CI), every fixture's cleanup
DELETE crawled — mid-flight at the 600 s deadline, then either swallowed
(requisition/vendor used `_ =`, silently leaving fixtures that broke the
next run on duplicate keys — observed locally as leftovers in tenants 0027/
0032) or blown past the package budget (CI round 1).

The fix (test code + one migration extension; no CI/workflow change, no
protocol change):

- **Migration 0014 gains two referential-integrity support indexes**:
  `purchase_orders_requisition_idx (requisition_id)` and
  `purchase_orders_vendor_idx (vendor_id)` — the FK check becomes an index
  lookup. The application only soft-deletes, so these close an integrity
  maintenance gap rather than serve a hot product query (documented in the
  migration and `docs/schema/procurement.md`).
- **`internal/platform/scalegate` (new, test-support)** — a session-level
  advisory lock (`pg_advisory_lock`, key "100K") on a dedicated pooled
  connection, serializing the four fixtures' LOAD phases (leading cleanup →
  seed → VACUUM/ANALYZE/CHECKPOINT). Measurement loops run after the release
  and overlap under their existing retry protocol (shapeAttempts=3) — the
  historically proven regime — instead of queueing (whole-body serialization
  re-busts the timeout from the other side: the queued package pays the wait
  plus its own run against 10m; observed locally, run 3). The release is
  idempotent (sync.Once) with a t.Cleanup safety net; a killed test binary
  releases the session lock with its connection.
- **Hardened cleanups** — dashboard/requisition/purchaseorder/vendor
  cleanupScale now fail loudly (t.Fatalf) if the delete fails twice instead
  of swallowing into leftover fixtures, and vendor/requisition gained
  leading cleanups so a stale fixture can never wedge the next seed on
  duplicate keys.
- **Dashboard fixture trimmed where no query under test looks**: approval
  steps are seeded only for the submitted (non-stepless) requisitions —
  2 × 20k = 40k rows instead of 2 × 80k = 160k; the pending count/page only
  read pending steps and their submitted parents; spend and distribution
  shapes are untouched (still 100k requisitions, 40k POs, 16 vendors, both
  windows weighted).

Evidence (this session): the exact CI shape — `go test -timeout 10m -count=1
./services/procurement/vendor/ ./services/procurement/requisition/
./services/procurement/purchaseorder/ ./services/procurement/dashboard/`
(four parallel test binaries, one database) — **all four ok in a 1m41s wall**
(vendor 99s, requisition 77s, purchase order 37s, dashboard 74s), with every
shape's median/p95 logged (14 measurement lines) and the database verified
leftover-free afterwards; the sequential full-suite sweep of all touched
packages plus server/gateway/rbac/scalegate — all ok; the extended 0014
applies cleanly on a fresh scratch database (5 indexes, schema_migrations 14,
dropped afterwards).

## Follow-ups

- **Redis cache for the dashboard aggregates (PRD §13: TTL 60 s,
  event-invalidated)** is deliberately not built — the measured p95 has
  ~160 ms headroom without it; adding cache invalidation wiring (no Redis
  read path exists in procurement yet) would have grown the diff past the
  issue's scope.
- **#56 (performance budgets)** owns the formal per-endpoint budget table;
  this issue's budget test claims the dashboard row only.
- **Demo loading** (`demo.*` → `procurement.*`) is the separate demo-epic
  decision; until it lands, the live dashboard reads the tenants the real
  APIs write, not the seed's demo schema.
- The pending page's RLS subplan (the steps policy deriving tenancy via the
  parent) costs ~half of that query; the platform-level policy reshaping
  flagged in #27/#32 would shave it — a platform decision, not a dashboard
  tweak.
- Pre-existing, unchanged (dev-notes 029–041): the procurement server test
  cleanup leak (`-p 1` and before/after wipes), `gofmt -l` flagging every
  file on this CRLF Windows checkout (new files are gofmt-clean), the
  `dbmigrate` advisory lock, `go test -race` unrunnable locally.
